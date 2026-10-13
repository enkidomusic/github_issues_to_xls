@echo off
setlocal
cd /d "%~dp0"

echo Generating roadmap from GitHub...
docker compose run --build --rm roadmap-generator
set "EXIT_CODE=%ERRORLEVEL%"
echo.
if "%EXIT_CODE%"=="0" (
  echo Finished. Check the res folder for the generated Excel file.
) else (
  echo Generation failed. Check the messages above.
)
echo.
pause
exit /b %EXIT_CODE%
