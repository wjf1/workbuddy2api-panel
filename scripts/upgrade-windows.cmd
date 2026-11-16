@echo off
setlocal EnableExtensions
rem ============================================================================
rem upgrade-windows.cmd -- in-place upgrade of the wb2api gateway on Windows
rem
rem Usage (run INSIDE the deploy directory, elevated):
rem   upgrade-windows.cmd <version-tag> [task-name]
rem   e.g.  upgrade-windows.cmd v1.22.0 wb2api
rem
rem What it does:
rem   1. download the official release zip + checksums.txt from GitHub Releases
rem      (resume supported: truncated downloads are a known hazard)
rem   2. verify sha256 against the CI-published checksums.txt -- abort on mismatch
rem   3. stop the scheduled task + kill the process
rem   4. back up wb2api.exe -> wb2api.exe.pre-<version>.bak
rem   5. extract and replace wb2api.exe, verify embedded FileVersion
rem   6. start the task again and verify /healthz
rem
rem Rollback if anything goes wrong:
rem   copy /Y wb2api.exe.pre-<version>.bak wb2api.exe
rem   schtasks /Run /TN <task-name>
rem
rem DRYRUN: set DRYRUN=1 to print every step without executing anything.
rem ============================================================================

rem ---- self-elevate (UAC is silent on hosts that pre-approve it) ----
net session >nul 2>&1
if %errorlevel% neq 0 (
  echo Requesting elevation...
  rem Pass ALL parameters explicitly: the elevated instance does NOT inherit
  rem process env vars set in the calling shell (learned the hard way -- a lost
  rem DRYRUN once took production down for ~45s on 2026-11-16, see HANDOFF).
  powershell -NoProfile -Command "Start-Process -FilePath '%~f0' -ArgumentList '%~1','%~2','%~3' -Verb RunAs"
  exit /b
)

set "VERSION=%~1"
set "TASK=%~2"
if "%TASK%"=="" set "TASK=wb2api"
set "DRYRUN=%~3"
set "DIR=%~dp0"
set "LOG=%DIR%upgrade-%VERSION%.log"
set "REPO=wjf1/workbuddy2api-panel"
set "ZIP=wb2api-panel-%VERSION%-windows-amd64.zip"

if "%VERSION%"=="" (
  echo Usage: upgrade-windows.cmd ^<version-tag^> [task-name] [1=dry-run]   e.g. upgrade-windows.cmd v1.22.0 wb2api 1
  exit /b 1
)
if not exist "%DIR%wb2api.exe" (
  echo FATAL: %DIR%wb2api.exe not found. Run this script from the deploy directory.
  exit /b 1
)

rem ---- HARD GATE (runs even in dry-run): the task must exist on this host ----
rem A wrong task name means the script is pointed at the wrong deployment;
rem stopping "nothing" would still kill wb2api.exe by image name and leave the
rem service unable to restart. Refuse early, before any download or stop.
schtasks /Query /TN "%TASK%" >nul 2>&1 || (
  echo FATAL: scheduled task "%TASK%" not found on this host. Check the task name before touching anything.
  exit /b 1
)

call :log "=== upgrade %VERSION% task=%TASK% dir=%DIR% dryrun=%DRYRUN% ==="

rem ---- step 1: download release assets ----
call :step "downloading %ZIP% + checksums.txt"
if "%DRYRUN%"=="1" goto :skipdl
set "BASE=https://github.com/%REPO%/releases/download/%VERSION%"
curl -sL --max-time 120 -C - -o "%TEMP%\%ZIP%" "%BASE%/%ZIP%" || (echo FATAL: download failed & exit /b 1)
curl -sL --max-time 30 -C - -o "%TEMP%\checksums-%VERSION%.txt" "%BASE%/checksums.txt" || (echo FATAL: checksum download failed & exit /b 1)
:skipdl

rem ---- step 2: verify sha256 (hard gate: never install an unverified binary) ----
call :step "verifying sha256"
if "%DRYRUN%"=="1" goto :skipsum
for /f "tokens=1" %%H in ('findstr /i "%ZIP%" "%TEMP%\checksums-%VERSION%.txt"') do set "EXPECT=%%H"
if "%EXPECT%"=="" (echo FATAL: %ZIP% not found in checksums.txt & exit /b 1)
for /f "skip=1 tokens=1" %%H in ('certutil -hashfile "%TEMP%\%ZIP%" SHA256') do if not defined GOT set "GOT=%%H"
if /i not "%GOT%"=="%EXPECT%" (
  echo FATAL: sha256 mismatch. got=%GOT% expect=%EXPECT% -- refusing to install.
  exit /b 1
)
call :log "sha256 OK: %GOT%"
:skipsum

rem ---- step 3: stop task + process ----
call :step "stopping task %TASK% and killing wb2api.exe (downtime starts)"
if "%DRYRUN%"=="1" goto :skipstop
schtasks /End /TN "%TASK%" >> "%LOG%" 2>&1
taskkill /F /IM wb2api.exe >> "%LOG%" 2>&1
ping -n 2 127.0.0.1 >nul
:skipstop

rem ---- step 4: backup current exe ----
call :step "backing up wb2api.exe -> wb2api.exe.pre-%VERSION%.bak"
if "%DRYRUN%"=="1" goto :skipbak
if not exist "%DIR%wb2api.exe.pre-%VERSION%.bak" copy /Y "%DIR%wb2api.exe" "%DIR%wb2api.exe.pre-%VERSION%.bak" >> "%LOG%" 2>&1
:skipbak

rem ---- step 5: extract + replace + verify embedded version ----
call :step "extracting and replacing wb2api.exe"
if "%DRYRUN%"=="1" goto :skipswap
powershell -NoProfile -Command "Expand-Archive -Force -Path '%TEMP%\%ZIP%' -DestinationPath '%TEMP%\wb2api-extract'" >> "%LOG%" 2>&1
if not exist "%TEMP%\wb2api-extract\wb2api.exe" (echo FATAL: extracted wb2api.exe missing & goto :rollback_now)
copy /Y "%TEMP%\wb2api-extract\wb2api.exe" "%DIR%wb2api.exe" >> "%LOG%" 2>&1
for /f "delims=" %%V in ('powershell -NoProfile -Command "(Get-Item '%DIR%wb2api.exe').VersionInfo.FileVersion"') do set "FV=%%V"
call :log "installed FileVersion=%FV%"
set "VERNUM=%VERSION:v=%"
set "VERNUM=%VERNUM:v=%"
echo %FV% | findstr /i "%VERNUM%" >nul || echo WARN: FileVersion "%FV%" does not contain expected version "%VERNUM%".
:skipswap

rem ---- step 6: start task + health check + AUTO-ROLLBACK ----
call :step "starting task %TASK% (downtime ends)"
if "%DRYRUN%"=="1" goto :skipstart
schtasks /Run /TN "%TASK%" >> "%LOG%" 2>&1
ping -n 4 127.0.0.1 >nul
curl -s --max-time 8 http://127.0.0.1:7863/healthz > "%TEMP%\wb-health.json" 2>&1
type "%TEMP%\wb-health.json"
echo.
findstr /c:"total" "%TEMP%\wb-health.json" >nul && (call :log "healthz OK -- upgrade complete.") || goto :rollback_now
goto :done

rem ---- auto-rollback: restore the backup exe and restart the task once ----
rem Never leave the service down on a failed swap -- the backup was taken from
rem a known-good running binary (step 4), so restoring it is always safe.
:rollback_now
echo.
echo [%TIME%] AUTO-ROLLBACK: restoring wb2api.exe.pre-%VERSION%.bak and restarting task...
call :log "AUTO-ROLLBACK: health check failed, restoring backup"
copy /Y "%DIR%wb2api.exe.pre-%VERSION%.bak" "%DIR%wb2api.exe" >> "%LOG%" 2>&1
schtasks /Run /TN "%TASK%" >> "%LOG%" 2>&1
ping -n 4 127.0.0.1 >nul
curl -s --max-time 8 http://127.0.0.1:7863/healthz > "%TEMP%\wb-health.json" 2>&1
type "%TEMP%\wb-health.json"
echo.
findstr /c:"total" "%TEMP%\wb-health.json" >nul && (call :log "rollback restart OK (running previous version)") || (
  echo MANUAL ACTION REQUIRED: service did not come back. Run in an elevated shell:
  echo   schtasks /Run /TN "%TASK%"
)
exit /b 1

:done
call :log "=== done ==="
exit /b 0

:step
echo [%TIME%] %~1
if "%DRYRUN%"=="1" echo   (dry-run: skipped)
exit /b

:log
echo [%DATE% %TIME%] %~1>> "%LOG%"
exit /b
