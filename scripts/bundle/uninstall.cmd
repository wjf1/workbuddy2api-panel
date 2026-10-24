@echo off
setlocal EnableExtensions
REM ============================================================
REM  wb2api-panel uninstaller
REM  Usage:  uninstall.cmd [taskname]    (default taskname: wb2api)
REM  Stops and DELETES the scheduled task. Deploy dir contents
REM  (wb2api.exe / config.json / auths\ / data\) are all KEPT.
REM ============================================================

net session >nul 2>&1
if errorlevel 1 (
  echo [INFO] Requesting administrator privileges ...
  powershell -NoProfile -Command "Start-Process -FilePath '%~f0' -ArgumentList '%~1' -Verb RunAs"
  exit /b 0
)

set "TASKNAME=%~1"
if "%TASKNAME%"=="" set "TASKNAME=wb2api"

echo [INFO] Stopping task "%TASKNAME%" ...
powershell -NoProfile -Command "Stop-ScheduledTask -TaskName '%TASKNAME%' -ErrorAction SilentlyContinue"
timeout /t 2 /nobreak >nul

echo [INFO] Deleting task "%TASKNAME%" (files/config/auths are KEPT) ...
schtasks /delete /tn "%TASKNAME%" /f >nul 2>&1
if errorlevel 1 (
  echo [WARN] Task not found or already deleted.
) else (
  echo [OK]   Task removed.
)

echo [DONE] Service removed. Deploy dir untouched:
echo        - wb2api.exe / config.json / auths\ / data\ all kept
if not defined WB2API_NOPAUSE pause
exit /b 0
