# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /src
RUN apk add --no-cache ca-certificates git

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/github-issues-to-xls .

# Runtime stage
FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /out/github-issues-to-xls /usr/local/bin/github-issues-to-xls
COPY repos.yaml /app/repos.yaml
COPY template.xlsx /app/template.xlsx

ENV TZ=Europe/Stockholm
RUN mkdir -p /app/res

ENTRYPOINT ["/usr/local/bin/github-issues-to-xls", "-config", "/app/repos.yaml", "-template", "/app/template.xlsx", "-out", "/app/res"]
