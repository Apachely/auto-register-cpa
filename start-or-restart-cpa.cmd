@echo off
setlocal
chcp 65001 >nul
title CPA Start or Restart

powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0cpa-service.ps1" -Action Restart -OpenBrowser
set "CPA_EXIT=%ERRORLEVEL%"

echo.
if "%CPA_EXIT%"=="0" (
  echo CPA backend and frontend are ready.
) else (
  echo CPA startup failed. Review the error and logs shown above.
)
echo.
pause
exit /b %CPA_EXIT%
