@echo off
setlocal
rem usage: post <channel> "message" [image.jpg]
rem reads WAPOST_URL and RELAY_TOKEN from %USERPROFILE%\.wapost.env

set "CFG=%USERPROFILE%\.wapost.env"
if not exist "%CFG%" (
  echo %CFG% not found. It needs WAPOST_URL=... and RELAY_TOKEN=... lines.
  exit /b 1
)
for /f "usebackq tokens=1,* delims==" %%a in ("%CFG%") do set "%%a=%%b"

if "%~1"=="" (
  echo usage: post ^<channel^> "message" [image.jpg]
  exit /b 1
)
set "CH=%~1"
set "MSG=%~2"

if "%~3"=="" (
  curl -s -X POST "%WAPOST_URL%/post?channel=%CH%" -H "Authorization: Bearer %RELAY_TOKEN%" --data-binary "%MSG%"
) else (
  curl -s -X POST "%WAPOST_URL%/post" -H "Authorization: Bearer %RELAY_TOKEN%" -F "channel=%CH%" -F "text=%MSG%" -F "image=@%~3"
)
echo.
