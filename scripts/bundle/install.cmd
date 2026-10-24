@echo off
setlocal EnableExtensions
REM ============================================================
REM  wb2api-panel one-shot installer (bundle build: 1.16.0-panel)
REM  Usage:  install.cmd [taskname]      (default taskname: wb2api)
REM
REM  What it does (idempotent, safe to re-run):
REM    1. self-elevate to Administrator
REM    2. if a scheduled task with the SAME name already exists:
REM       stop it and back up its definition XML next to this script
REM       (previous-task-<name>.xml)
REM    3. stop any leftover wb2api.exe process (exact name only,
REM       never wildcards)
REM    4. register a SYSTEM scheduled task pointing at THIS directory
REM       (wb2api.exe -config config.json), boot autostart +30s,
REM       restart on failure x3, no execution time limit
REM    5. start the task, wait for /healthz, print panel URL + api_key
REM
REM  Notes:
REM    - config.json: if absent, wb2api.exe auto-generates one with a
REM      random api_key on first start (listen :7863).
REM    - auths/: NOT migrated automatically. Import accounts via the
REM      panel (Add account / Import JSON). One account must exist on
REM      ONE machine only - duplicated copies get their refresh token
REM      chains revoked by upstream (see README-B.md).
REM    - pause at the end is skipped when WB2API_NOPAUSE is set.
REM ============================================================

REM --- self-elevate ----------------------------------------------
net session >nul 2>&1
if errorlevel 1 (
  echo [INFO] Requesting administrator privileges ...
  powershell -NoProfile -Command "Start-Process -FilePath '%~f0' -ArgumentList '%~1' -Verb RunAs"
  exit /b 0
)

set "TASKNAME=%~1"
if "%TASKNAME%"=="" set "TASKNAME=wb2api"
set "DIR=%~dp0"
set "EXE=%DIR%wb2api.exe"
set "PORT=7863"
if exist "%DIR%config.json" (
  for /f "usebackq delims=" %%P in (`powershell -NoProfile -Command "$c='%DIR%config.json'; try { $l = (Get-Content -LiteralPath $c -Raw | ConvertFrom-Json).listen } catch {}; if (-not $l) { $l = ':7863' }; ($l -split ':')[-1]"`) do set "PORT=%%P"
)
set "PANELURL=http://127.0.0.1:%PORT%/panel/"

if not exist "%EXE%" (
  echo [ERROR] wb2api.exe not found in %DIR%
  call :end 1
  exit /b 1
)

echo [INFO] Deploy dir : %DIR%
echo [INFO] Task name  : %TASKNAME%
echo [INFO] Binary     : wb2api.exe 1.16.0-panel
echo.

REM --- stop + backup + replace same-name task ---------------------
schtasks /query /tn "%TASKNAME%" >nul 2>&1
if not errorlevel 1 (
  echo [INFO] Existing task "%TASKNAME%" found - stopping and replacing ...
  powershell -NoProfile -Command "Stop-ScheduledTask -TaskName '%TASKNAME%' -ErrorAction SilentlyContinue"
  schtasks /query /tn "%TASKNAME%" /xml > "%DIR%previous-task-%TASKNAME%.xml" 2>nul
  echo [INFO] Previous task definition backed up: previous-task-%TASKNAME%.xml
  timeout /t 2 /nobreak >nul
)

REM leftover guard: kill ONLY processes whose ExecutablePath is THIS
REM deploy dir's wb2api.exe (never by image name - another instance
REM on this machine may be a different deployment)
powershell -NoProfile -Command "Get-CimInstance Win32_Process -Filter \"Name='wb2api.exe'\" | Where-Object { $_.ExecutablePath -eq '%EXE%' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }"
if not errorlevel 1 timeout /t 1 /nobreak >nul

REM --- register task (SYSTEM, boot autostart +30s, restart x3) ----
echo [INFO] Registering scheduled task ...
powershell -NoProfile -Command "$a = New-ScheduledTaskAction -Execute '%EXE%' -Argument '-config config.json' -WorkingDirectory '%DIR%'; $t = New-ScheduledTaskTrigger -AtStartup; $t.Delay = 'PT30S'; $p = New-ScheduledTaskPrincipal -UserId 'S-1-5-18' -LogonType ServiceAccount -RunLevel Highest; $s = New-ScheduledTaskSettingsSet -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit ([TimeSpan]::Zero) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries; Register-ScheduledTask -TaskName '%TASKNAME%' -Action $a -Trigger $t -Principal $p -Settings $s -Force | Out-Null"
if errorlevel 1 (
  echo [ERROR] Failed to register scheduled task.
  call :end 1
  exit /b 1
)
echo [INFO] Task "%TASKNAME%" registered.

REM --- start + health self-check ----------------------------------
powershell -NoProfile -Command "Start-ScheduledTask -TaskName '%TASKNAME%'"
echo [INFO] Task started, waiting for /healthz ...

set /a TRIES=0
:wait_health
timeout /t 2 /nobreak >nul
set /a TRIES+=1
curl -s -m 3 http://127.0.0.1:%PORT%/healthz 2>nul | find /i "workbuddy2api" >nul
if not errorlevel 1 goto healthy
if %TRIES% lss 10 goto wait_health
echo [WARN] /healthz not reachable after 20s on port %PORT% - check port conflicts or server.log
goto print_info

:healthy
echo [OK]   Service is healthy.

:print_info
set "APIKEY="
if exist "%DIR%config.json" (
  for /f "usebackq delims=" %%K in (`powershell -NoProfile -Command "(Get-Content -LiteralPath '%DIR%config.json' -Raw | ConvertFrom-Json).api_key"`) do set "APIKEY=%%K"
)
echo.
echo ============================================================
echo  Panel    : %PANELURL%
if defined APIKEY (echo  API key  : %APIKEY%) else (echo  API key  : ^(empty - auth disabled; set api_key before exposing beyond localhost^))
if not exist "%DIR%config.json" echo  Config   : auto-generated on first start (see above)
echo  Config   : %DIR%config.json
echo  Next     : open the panel, import accounts (Add account /
echo             Import JSON). ONE account on ONE machine only!
echo ============================================================
call :end 0
exit /b 0

:end
if not defined WB2API_NOPAUSE pause
exit /b %~1
