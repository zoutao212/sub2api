@echo off
REM ============================================================
REM  sub2api rebuild: current source -> new exe -> restart
REM
REM  Prerequisite: sync-upstream.bat has already been run.
REM  Works directly from git: no make (absent on Windows) and no
REM  pnpm (absent from PATH here) - it calls node_modules/.bin.
REM
REM  Why this exists: git pull only updates source. sub2api serves
REM  the frontend from inside the binary (backend/internal/web/dist
REM  embedded via -tags embed), so until a new exe is built the
REM  previously compiled code keeps running.
REM ============================================================
setlocal EnableExtensions
cd /d "%~dp0"

set "EXE=%CD%\backend\sub2api.exe"
set "NEWEXE=%CD%\backend\sub2api.exe.new"
set "DIST=%CD%\backend\internal\web\dist"
set "VITE=%CD%\frontend\node_modules\.bin\vite.cmd"
for /f %%i in ('powershell -NoProfile -Command "Get-Date -Format yyyyMMdd-HHmmss"') do set "STAMP=%%i"
if not defined STAMP set "STAMP=manual"

echo ============================================================
echo  sub2api rebuild
echo ============================================================
echo.

REM ---------- 1) preflight ----------
echo [1/6] Preflight...
where go >nul 2>&1
if errorlevel 1 (
    echo [FAIL] 'go' is not on PATH.
    exit /b 1
)
if not exist "%VITE%" (
    echo [FAIL] frontend dependencies missing: %VITE%
    echo        Install them with pnpm install, then re-run.
    exit /b 1
)
if not exist "%CD%\backend\cmd\server\VERSION" (
    echo [FAIL] backend\cmd\server\VERSION not found - wrong branch?
    exit /b 1
)
set "SRCVER="
for /f "usebackq delims=" %%v in ("%CD%\backend\cmd\server\VERSION") do set "SRCVER=%%v"
if not defined SRCVER (
    echo [FAIL] could not read the VERSION file.
    exit /b 1
)
echo        source VERSION  = %SRCVER%
if exist "%EXE%" for %%f in ("%EXE%") do echo        current exe     = %%~tF  %%~zF bytes
echo.

REM ---------- 2) stop the running service ----------
echo [2/6] Stopping sub2api if it is running...
tasklist /FI "IMAGENAME eq sub2api.exe" 2>nul | findstr /I "sub2api.exe" >nul
if not errorlevel 1 (
    echo        found - stopping it...
    taskkill /F /IM sub2api.exe >nul 2>&1
    call :WaitStopped
    if errorlevel 1 (
        echo [FAIL] sub2api.exe is still running; the exe file is locked.
        exit /b 1
    )
) else (
    echo        not running
)
echo.

REM ---------- 3) frontend build ----------
echo [3/6] Building frontend (vite runs vue-tsc as a gate)...
echo        this takes a while; output goes to backend\internal\web\dist
pushd "%CD%\frontend"
call "%VITE%" build
if errorlevel 1 (
    popd
    echo.
    echo [FAIL] frontend build failed.
    echo        NOTE: vite empties the outDir before writing, so dist may
    echo        now be incomplete. The old exe has NOT been replaced, so
    echo        the deployed build still works. Fix the errors above and
    echo        re-run this script.
    exit /b 1
)
popd
if not exist "%DIST%\index.html" (
    echo [FAIL] %DIST%\index.html missing - aborting before the Go build.
    exit /b 1
)
echo        dist OK.
echo.

REM ---------- 4) backend build ----------
echo [4/6] Building backend (-tags embed is required; without it the
echo       binary has no frontend and the UI will not load)...
echo       a first build after a Go version bump downloads a toolchain
echo       (~70-100 MB) and needs network access.
pushd "%CD%\backend"
set CGO_ENABLED=0
go build -tags embed -trimpath -o "%NEWEXE%" ./cmd/server
if errorlevel 1 (
    popd
    echo [FAIL] go build failed. Old exe untouched and still deployable.
    exit /b 1
)
popd
if not exist "%NEWEXE%" (
    echo [FAIL] %NEWEXE% was not produced.
    exit /b 1
)
echo        build OK.
echo.

REM ---------- 5) swap ----------
echo [5/6] Replacing the exe (keeping a timestamped backup)...
if exist "%EXE%" move /Y "%EXE%" "%EXE%.bak-%STAMP%" >nul
if errorlevel 1 (
    echo [FAIL] could not move the old exe aside - is it locked?
    exit /b 1
)
move /Y "%NEWEXE%" "%EXE%" >nul
if errorlevel 1 (
    echo [FAIL] could not install the new exe.
    exit /b 1
)
echo        backup: backend\sub2api.exe.bak-%STAMP%
echo.

REM ---------- 6) restart ----------
echo [6/6] Restarting through start-sub2api.bat...
call "%CD%\start-sub2api.bat"
echo.
echo ============================================================
echo  Rebuild complete. Source VERSION is %SRCVER%.
echo  The admin page footer should now report %SRCVER%.
echo  If the browser still shows the old version, it is cache -
echo  press Ctrl+F5.
echo
echo  If the service fails to come up, roll back with:
echo    copy /Y backend\sub2api.exe.bak-%STAMP% backend\sub2api.exe
echo ============================================================
goto :done

REM ---------- subroutines ----------
:WaitStopped
for /L %%i in (1,1,15) do (
    timeout /t 1 /nobreak >nul
    tasklist /FI "IMAGENAME eq sub2api.exe" 2>nul | findstr /I "sub2api.exe" >nul
    if errorlevel 1 exit /b 0
)
exit /b 1

:done
endlocal
pause
