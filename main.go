package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xuri/excelize/v2"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Repos []RepoFilter `yaml:"repos"`
}
type RepoFilter struct {
	Repo   string `yaml:"repo"`   // owner/name
	Filter string `yaml:"filter"` // строка поиска GitHub, например "is:issue state:open"
}
type SubIssuesSummary struct {
	Total int `json:"total"`
}
type User struct {
	Login string `json:"login"`
}
type Issue struct {
	Number           int               `json:"number"`
	Title            string            `json:"title"`
	HTMLURL          string            `json:"html_url"`
	URL              string            `json:"url"` // API-URL задачи, на его основе строятся /parent, /sub_issues и т.д.
	State            string            `json:"state"`
	Assignees        []User            `json:"assignees"`
	SubIssuesSummary *SubIssuesSummary `json:"sub_issues_summary"`
}
type SearchResponse struct {
	TotalCount int     `json:"total_count"`
	Items      []Issue `json:"items"`
}

// issueFieldValue — значение issue field (REST: GET /issues/{n}/issue-field-values).
type issueFieldValue struct {
	Name         string `json:"issue_field_name"`
	DataType     string `json:"data_type"`
	Value        any    `json:"value"`
	SingleSelect *struct {
		Name string `json:"name"`
	} `json:"single_select_option"`
	MultiSelect []struct {
		Name string `json:"name"`
	} `json:"multi_select_options"`
}

func (v issueFieldValue) String() string {
	if v.SingleSelect != nil {
		return v.SingleSelect.Name
	}
	if len(v.MultiSelect) > 0 {
		names := make([]string, 0, len(v.MultiSelect))
		for _, o := range v.MultiSelect {
			names = append(names, o.Name)
		}
		return strings.Join(names, ", ")
	}
	return formatValue(v.Value)
}
func formatValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(t)
	}
}

// GitHub — клиент API с кэшем, чтобы не запрашивать одно и то же дважды.
type GitHub struct {
	client   *http.Client
	token    string
	parents  map[string]*Issue            // API-URL задачи -> родитель (nil, если родителя нет)
	children map[string][]*Issue          // API-URL задачи -> прямые дочерние задачи
	fields   map[string]map[string]string // API-URL задачи -> поля (ключ — имя поля в нижнем регистре)
	warned   map[string]bool
}

func NewGitHub(token string) *GitHub {
	return &GitHub{
		client:   &http.Client{Timeout: 20 * time.Second},
		token:    token,
		parents:  map[string]*Issue{},
		children: map[string][]*Issue{},
		fields:   map[string]map[string]string{},
		warned:   map[string]bool{},
	}
}

// warn печатает предупреждение в stderr один раз для каждого уникального текста.
func (g *GitHub) warn(msg string) {
	if g.warned[msg] {
		return
	}
	g.warned[msg] = true
	fmt.Fprintln(os.Stderr, "Предупреждение:", msg)
}

// get выполняет GET и декодирует JSON в out. Возвращает HTTP-статус, чтобы вызывающий мог отличить 404.
func (g *GitHub) get(apiURL string, out any) (int, error) {
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("API вернуло статус %d для %s", resp.StatusCode, apiURL)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return resp.StatusCode, fmt.Errorf("ошибка JSON для %s: %w", apiURL, err)
	}
	return resp.StatusCode, nil
}
func (g *GitHub) searchIssues(repo, filter string) ([]Issue, error) {
	query := fmt.Sprintf("repo:%s %s", repo, filter)
	var all []Issue
	for page := 1; ; page++ {
		apiURL := fmt.Sprintf(
			"https://api.github.com/search/issues?q=%s&per_page=100&page=%d",
			url.QueryEscape(query), page,
		)
		var res SearchResponse
		if _, err := g.get(apiURL, &res); err != nil {
			return nil, fmt.Errorf("страница %d: %w", page, err)
		}
		if len(res.Items) == 0 {
			break
		}
		all = append(all, res.Items...)
		if len(all) >= res.TotalCount {
			break
		}
		// Search API имеет жёсткие лимиты — делаем паузу между страницами
		time.Sleep(500 * time.Millisecond)
	}
	return all, nil
}

// parentOf возвращает родителя задачи или nil, если родителя нет (API отвечает 404).
func (g *GitHub) parentOf(issue *Issue) (*Issue, error) {
	if p, ok := g.parents[issue.URL]; ok {
		return p, nil
	}
	var parent Issue
	status, err := g.get(issue.URL+"/parent", &parent)
	if status == http.StatusNotFound {
		g.parents[issue.URL] = nil
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	g.parents[issue.URL] = &parent
	return &parent, nil
}

// childrenOf возвращает прямые sub-issues задачи (со всех страниц).
func (g *GitHub) childrenOf(issue *Issue) ([]*Issue, error) {
	if c, ok := g.children[issue.URL]; ok {
		return c, nil
	}
	// Если API уже сообщил, что дочерних нет, лишний запрос не нужен.
	if issue.SubIssuesSummary != nil && issue.SubIssuesSummary.Total == 0 {
		g.children[issue.URL] = nil
		return nil, nil
	}
	var all []*Issue
	for page := 1; ; page++ {
		var batch []Issue
		apiURL := fmt.Sprintf("%s/sub_issues?per_page=100&page=%d", issue.URL, page)
		if _, err := g.get(apiURL, &batch); err != nil {
			return nil, err
		}
		for i := range batch {
			all = append(all, &batch[i])
		}
		if len(batch) < 100 {
			break
		}
	}
	g.children[issue.URL] = all
	return all, nil
}

// ancestors возвращает цепочку предков от корня до непосредственного родителя (без самой задачи).
func (g *GitHub) ancestors(issue *Issue) ([]*Issue, error) {
	var chain []*Issue
	seen := map[string]bool{issue.URL: true}
	cur := issue
	for {
		p, err := g.parentOf(cur)
		if err != nil {
			return nil, err
		}
		if p == nil || seen[p.URL] {
			break
		}
		seen[p.URL] = true
		chain = append([]*Issue{p}, chain...) // кладём в начало: корень окажется первым
		cur = p
	}
	return chain, nil
}

// paths обходит потомков в глубину и вызывает emit для каждого пути от корня до листа (включительно).
func (g *GitHub) paths(issue *Issue, prefix []*Issue, onPath map[string]bool, emit func([]*Issue)) error {
	path := append(append([]*Issue{}, prefix...), issue)
	kids, err := g.childrenOf(issue)
	if err != nil {
		return err
	}
	if len(kids) == 0 {
		emit(path)
		return nil
	}
	onPath[issue.URL] = true
	defer delete(onPath, issue.URL)
	for _, kid := range kids {
		if onPath[kid.URL] { // защита от циклов
			continue
		}
		if err := g.paths(kid, path, onPath, emit); err != nil {
			return err
		}
	}
	return nil
}

// ---------- Поля: issue fields (REST) и поля проектов (GraphQL, Projects V2) ----------
const projectFieldsQuery = `
query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    issue(number: $number) {
      projectItems(first: 20) {
        nodes {
          fieldValues(first: 50) {
            nodes {
              __typename
              ... on ProjectV2ItemFieldDateValue { date field { ... on ProjectV2FieldCommon { name } } }
              ... on ProjectV2ItemFieldTextValue { text field { ... on ProjectV2FieldCommon { name } } }
              ... on ProjectV2ItemFieldNumberValue { number field { ... on ProjectV2FieldCommon { name } } }
              ... on ProjectV2ItemFieldSingleSelectValue { name field { ... on ProjectV2FieldCommon { name } } }
              ... on ProjectV2ItemFieldIterationValue { title field { ... on ProjectV2FieldCommon { name } } }
            }
          }
        }
      }
    }
  }
}`

type projectResponse struct {
	Data struct {
		Repository *struct {
			Issue *struct {
				ProjectItems struct {
					Nodes []struct {
						FieldValues struct {
							Nodes []struct {
								Typename string   `json:"__typename"`
								Date     string   `json:"date"`
								Text     string   `json:"text"`
								Number   *float64 `json:"number"`
								Name     string   `json:"name"`  // single select
								Title    string   `json:"title"` // iteration
								Field    struct {
									Name string `json:"name"`
								} `json:"field"`
							} `json:"nodes"`
						} `json:"fieldValues"`
					} `json:"nodes"`
				} `json:"projectItems"`
			} `json:"issue"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// parseIssueURL разбирает https://api.github.com/repos/{owner}/{repo}/issues/{n}.
func parseIssueURL(apiURL string) (owner, repo string, number int, err error) {
	parts := strings.Split(strings.TrimPrefix(apiURL, "https://api.github.com/repos/"), "/")
	if len(parts) != 4 || parts[2] != "issues" {
		return "", "", 0, fmt.Errorf("неожиданный формат URL задачи: %s", apiURL)
	}
	number, err = strconv.Atoi(parts[3])
	return parts[0], parts[1], number, err
}

// projectFields возвращает значения полей задачи во всех её проектах. Первое найденное значение поля побеждает.
func (g *GitHub) projectFields(issue *Issue) map[string]string {
	result := map[string]string{}
	if g.token == "" {
		return result // GraphQL API требует авторизации
	}
	owner, repo, number, err := parseIssueURL(issue.URL)
	if err != nil {
		g.warn(err.Error())
		return result
	}
	body, _ := json.Marshal(map[string]any{
		"query":     projectFieldsQuery,
		"variables": map[string]any{"owner": owner, "name": repo, "number": number},
	})
	req, err := http.NewRequest("POST", "https://api.github.com/graphql", bytes.NewReader(body))
	if err != nil {
		g.warn(err.Error())
		return result
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		g.warn(fmt.Sprintf("GraphQL-запрос для #%d: %v", issue.Number, err))
		return result
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		g.warn(fmt.Sprintf("GraphQL вернул статус %d (проверьте scope read:project у токена)", resp.StatusCode))
		return result
	}
	var res projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		g.warn(fmt.Sprintf("ошибка JSON в ответе GraphQL: %v", err))
		return result
	}
	for _, e := range res.Errors {
		g.warn("GraphQL: " + e.Message)
	}
	if res.Data.Repository == nil || res.Data.Repository.Issue == nil {
		return result
	}
	for _, item := range res.Data.Repository.Issue.ProjectItems.Nodes {
		for _, fv := range item.FieldValues.Nodes {
			name := strings.ToLower(strings.TrimSpace(fv.Field.Name))
			if name == "" {
				continue
			}
			var val string
			switch fv.Typename {
			case "ProjectV2ItemFieldDateValue":
				val = fv.Date
			case "ProjectV2ItemFieldTextValue":
				val = fv.Text
			case "ProjectV2ItemFieldNumberValue":
				if fv.Number != nil {
					val = strconv.FormatFloat(*fv.Number, 'f', -1, 64)
				}
			case "ProjectV2ItemFieldSingleSelectValue":
				val = fv.Name
			case "ProjectV2ItemFieldIterationValue":
				val = fv.Title
			}
			if _, exists := result[name]; val != "" && !exists {
				result[name] = val
			}
		}
	}
	return result
}

// loadFields собирает все поля задачи: сначала issue fields, затем поля проектов
// (проектные значения не перезаписывают уже найденные в issue fields).
func (g *GitHub) loadFields(issue *Issue) map[string]string {
	if f, ok := g.fields[issue.URL]; ok {
		return f
	}
	f := map[string]string{}
	var vals []issueFieldValue
	status, err := g.get(issue.URL+"/issue-field-values", &vals)
	if err != nil && status != http.StatusNotFound { // 404 — issue fields недоступны для этого репозитория
		g.warn(fmt.Sprintf("issue fields для #%d: %v", issue.Number, err))
	}
	for _, v := range vals {
		if s := v.String(); s != "" {
			f[strings.ToLower(strings.TrimSpace(v.Name))] = s
		}
	}
	for name, val := range g.projectFields(issue) {
		if _, exists := f[name]; !exists {
			f[name] = val
		}
	}
	g.fields[issue.URL] = f
	return f
}

// assignee возвращает логин первого assignee листа; если его нет — первого найденного
// при подъёме по родителям (от ближайшего к корню). Если нигде нет — пустую строку.
func assignee(path []*Issue) string {
	for i := len(path) - 1; i >= 0; i-- {
		if len(path[i].Assignees) > 0 {
			return path[i].Assignees[0].Login
		}
	}
	return ""
}

// clean убирает символы, ломающие табличный вывод.
func clean(s string) string {
	return strings.NewReplacer("\t", " ", "\r", " ", "\n", " ").Replace(s)
}

// printTable выводит таблицу с заголовками и автоматически подбирает ширину колонок.
func printTable(rows [][]string) {
	widths := make([]int, len(columns))
	for i, col := range columns {
		widths[i] = utf8.RuneCountInString(col)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && utf8.RuneCountInString(cell) > widths[i] {
				widths[i] = utf8.RuneCountInString(cell)
			}
		}
	}
	printBorder := func() {
		fmt.Print("+")
		for _, width := range widths {
			fmt.Print(strings.Repeat("-", width+2), "+")
		}
		fmt.Println()
	}
	printRow := func(row []string) {
		fmt.Print("|")
		for i, width := range widths {
			value := ""
			if i < len(row) {
				value = row[i]
			}
			padding := width - utf8.RuneCountInString(value)
			fmt.Printf(" %s%s |", value, strings.Repeat(" ", padding))
		}
		fmt.Println()
	}
	printBorder()
	printRow(columns)
	printBorder()
	for _, row := range rows {
		printRow(row)
	}
	printBorder()
}

var columns = []string{"№№ п/п", "Задачи и подзадачи", "Ответственный (студент)", "Исполнитель (студент)", "Стоимость задачи в баллах", "План. старт", "План. финиш", "Факт. старт", "Факт. финиш", "Задача выполнена"}

// row формирует строку таблицы (колонки разделены табуляцией) для пути root → ... → leaf.
func (g *GitHub) row(path []*Issue) string {
	leaf := path[len(path)-1]
	titles := make([]string, len(path))
	for i, it := range path {
		titles[i] = clean(it.Title)
	}
	f := g.loadFields(leaf)
	leafAssignee := ""
	if len(leaf.Assignees) > 0 {
		leafAssignee = leaf.Assignees[0].Login
	}
	completed := "false"
	if leaf.State == "closed" {
		completed = "true"
	}
	return strings.Join([]string{
		strings.Join(titles, ": "),
		assignee(path),
		leafAssignee,
		f["effort"],
		f["start date"],
		f["target date"],
		f["actual start"],
		f["actual end"],
		completed,
	}, "\t")
}
func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	for i, r := range cfg.Repos {
		if r.Repo == "" {
			return nil, fmt.Errorf("repos[%d]: не указан repo", i)
		}
	}
	return &cfg, nil
}

// loadStudentNames читает соответствие GitHub Login -> ФИО с листа «Список студентов».
// Заголовки и пустые строки пропускаются, поэтому таблица может расширяться.
func loadStudentNames(f *excelize.File) (map[string]string, error) {
	sheet := "Список студентов"
	sheetIndex, err := f.GetSheetIndex(sheet)
	if err != nil {
		return nil, fmt.Errorf("не удалось получить индекс листа %q: %w", sheet, err)
	}
	if sheetIndex == -1 {
		return nil, fmt.Errorf("в шаблоне отсутствует лист %q", sheet)
	}
	rows, err := f.GetRows(sheet)
	if err != nil {
		return nil, err
	}
	names := make(map[string]string)
	for i, row := range rows {
		if i < 2 || len(row) < 3 {
			continue
		}
		fullName := strings.TrimSpace(row[1]) // колонка B: ФИО
		login := strings.TrimSpace(row[2])    // колонка C: GitHub Login
		if login == "" || fullName == "" || strings.EqualFold(login, "GitHub Login") ||
			strings.EqualFold(fullName, "ФИО") {
			continue
		}
		names[strings.ToLower(login)] = fullName
	}
	return names, nil
}

func displayStudent(login string, names map[string]string) string {
	login = strings.TrimSpace(login)
	if login == "" {
		return ""
	}
	if name, ok := names[strings.ToLower(login)]; ok && name != "" {
		return name
	}
	return login
}

// excelDate преобразует дату из GitHub в значение даты Excel, а не в текст.
// Это важно для корректной работы формул разности дат.
func excelDate(value string) (any, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, true
	}
	for _, layout := range []string{"2006-01-02", "2006-01-02T15:04:05Z", "2006-01-02T15:04:05-07:00", "02.01.2006", "02.01.06", "2006/01/02"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, true
		}
	}
	return value, false
}

func stringPtr(s string) *string { return &s }

// writeRoadmap копирует шаблон в новый файл, очищает данные A:L ниже заголовков
// и заполняет дорожную карту. Формульные столбцы H и K сохраняются/восстанавливаются.
func writeRoadmap(templatePath, outputDir string, rows [][]string) (string, error) {
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return "", fmt.Errorf("не удалось создать каталог %s: %w", outputDir, err)
	}
	stamp := time.Now().Format("20060102_150405")
	outputPath := outputDir + string(os.PathSeparator) + "roadmap_" + stamp + ".xlsx"
	template, err := os.ReadFile(templatePath)
	if err != nil {
		return "", fmt.Errorf("не удалось прочитать шаблон %s: %w", templatePath, err)
	}
	if err := os.WriteFile(outputPath, template, 0644); err != nil {
		return "", fmt.Errorf("не удалось создать выходной файл %s: %w", outputPath, err)
	}

	f, err := excelize.OpenFile(outputPath)
	if err != nil {
		return "", fmt.Errorf("не удалось открыть Excel-шаблон: %w", err)
	}
	defer f.Close()

	names, err := loadStudentNames(f)
	if err != nil {
		return "", err
	}

	sheet := "Дорожная карта"
	sheetIndex, err := f.GetSheetIndex(sheet)
	if err != nil {
		return "", fmt.Errorf("не удалось получить индекс листа %q: %w", sheet, err)
	}
	if sheetIndex == -1 {
		return "", fmt.Errorf("в шаблоне отсутствует лист %q", sheet)
	}
	existingRows, err := f.GetRows(sheet)
	if err != nil {
		return "", err
	}
	// В шаблоне строка 1 — заголовок, строка 2 — подзаголовки; данные начинаются со строки 3.
	lastRow := len(existingRows)
	requiredLastRow := len(rows) + 2
	if requiredLastRow > lastRow {
		lastRow = requiredLastRow
	}
	for r := 3; r <= lastRow; r++ {
		for c := 1; c <= 12; c++ {
			cell, _ := excelize.CoordinatesToCellName(c, r)
			if err := f.SetCellValue(sheet, cell, ""); err != nil {
				return "", err
			}
		}
	}

	// Переносим оформление строки данных шаблона на новые строки, если данных больше, чем было.
	for r := 3; r <= requiredLastRow; r++ {
		if r > len(existingRows) && r > 3 {
			for c := 1; c <= 12; c++ {
				cell, _ := excelize.CoordinatesToCellName(c, r)
				sourceCell, _ := excelize.CoordinatesToCellName(c, 3)
				styleID, styleErr := f.GetCellStyle(sheet, sourceCell)
				if styleErr == nil && styleID != 0 {
					_ = f.SetCellStyle(sheet, cell, cell, styleID)
				}
			}
		}
	}

	for i, row := range rows {
		r := i + 3
		// row: цепочка, ответственный login, исполнитель login, effort,
		// плановый старт, плановый финиш, факт старт, факт финиш, выполнена.
		values := []any{
			i + 1,
			row[0],
			displayStudent(row[1], names),
			displayStudent(row[2], names),
			row[3],
		}
		for j, value := range values {
			cell, _ := excelize.CoordinatesToCellName(j+1, r)
			if err := f.SetCellValue(sheet, cell, value); err != nil {
				return "", err
			}
		}
		// Формулы длительности из шаблона, адаптированные под номер строки.
		h, _ := excelize.CoordinatesToCellName(8, r)
		k, _ := excelize.CoordinatesToCellName(11, r)
		if err := f.SetCellFormula(sheet, h, fmt.Sprintf(`IF(OR(F%d="",G%d=""),"",IF(G%d-F%d=0,"",G%d-F%d))`, r, r, r, r, r, r)); err != nil {
			return "", err
		}
		// Записываем даты как числовые даты Excel, чтобы формулы вычитания работали.
		dateColumns := []struct {
			col   int
			value string
		}{
			{6, row[4]}, {7, row[5]}, {9, row[6]}, {10, row[7]},
		}
		for _, item := range dateColumns {
			cell, _ := excelize.CoordinatesToCellName(item.col, r)
			dateValue, isDate := excelDate(item.value)
			if isDate {
				if err := f.SetCellValue(sheet, cell, dateValue); err != nil {
					return "", err
				}
			} else if err := f.SetCellValue(sheet, cell, item.value); err != nil {
				return "", err
			}
			// Берём исходный стиль ячейки шаблона и меняем только формат даты.
			styleSource, _ := excelize.CoordinatesToCellName(item.col, 3)
			styleID, styleErr := f.GetCellStyle(sheet, styleSource)
			if styleErr == nil {
				baseStyle, getStyleErr := f.GetStyle(styleID)
				if getStyleErr == nil && baseStyle != nil {
					format := "dd.mm.yyyy"
					baseStyle.CustomNumFmt = &format
					newStyle, newStyleErr := f.NewStyle(baseStyle)
					if newStyleErr == nil {
						_ = f.SetCellStyle(sheet, cell, cell, newStyle)
					}
				}
			}
		}
		if err := f.SetCellFormula(sheet, k, fmt.Sprintf(`IF(OR(I%d="",J%d=""),"",IF(J%d-I%d=0,"",J%d-I%d))`, r, r, r, r, r, r)); err != nil {
			return "", err
		}
		completed := strings.EqualFold(row[8], "true")
		l, _ := excelize.CoordinatesToCellName(12, r)
		if err := f.SetCellValue(sheet, l, completed); err != nil {
			return "", err
		}
	}

	if err := f.Save(); err != nil {
		return "", fmt.Errorf("не удалось сохранить Excel-файл: %w", err)
	}
	return outputPath, nil
}

func main() {
	configPath := flag.String("config", "repos.yaml", "путь к YAML-файлу со списком репозиториев и фильтров")
	templatePath := flag.String("template", "template.xlsx", "путь к Excel-шаблону")
	outputDir := flag.String("out", "res", "каталог для готовых Excel-файлов")
	flag.Parse()
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка чтения конфига %s: %v\n", *configPath, err)
		os.Exit(1)
	}
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "Предупреждение: GITHUB_TOKEN не задан, приватные репозитории и поля проектов будут недоступны")
	}
	gh := NewGitHub(token)
	printed := map[string]bool{} // одна и та же строка не должна выводиться дважды
	var rows [][]string
	for _, r := range cfg.Repos {
		fmt.Fprintf(os.Stderr, "\n=== %s ===\nФильтр: %s\n", r.Repo, r.Filter)
		issues, err := gh.searchIssues(r.Repo, r.Filter)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Ошибка: %v\n", err)
			continue
		}
		fmt.Fprintf(os.Stderr, "Найдено задач: %d\n", len(issues))
		for i := range issues {
			issue := &issues[i]
			up, err := gh.ancestors(issue)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Ошибка (#%d %s): %v\n", issue.Number, issue.Title, err)
				continue
			}
			err = gh.paths(issue, up, map[string]bool{}, func(path []*Issue) {
				line := gh.row(path)
				if !printed[line] {
					printed[line] = true
					rows = append(rows, strings.Split(line, "\t"))
				}
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "Ошибка (#%d %s): %v\n", issue.Number, issue.Title, err)
			}
		}
		// Пауза между репозиториями, чтобы не упереться в rate limit поиска
		time.Sleep(time.Second)
	}
	outputPath, err := writeRoadmap(*templatePath, *outputDir, rows)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка формирования Excel-файла: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Готово. Excel-файл сохранён: %s\n", outputPath)
}
