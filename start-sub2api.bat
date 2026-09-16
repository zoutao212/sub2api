@echo off
setlocal EnableExtensions
set "BASE=%~dp0"
set "LOGDIR=%BASE%backend\data\logs"
set "LOG=%LOGDIR%\sub2api.log"
set "URL=http://localhost:8080"

rem ============================================================
rem  Sub2API 一键启动器
rem  1. 依赖服务未运行时自动拉起，必要时请求管理员提权
rem  2. sub2api 以隐藏窗口方式独立启动，不残留黑窗
rem  3. 就绪后自动打开浏览器，窗口自动关闭
rem ============================================================

rem ---------- 0) 仅当需要启动系统服务时才提权 ----------
net session >nul 2>&1
if errorlevel 1 (
    set "NEEDADMIN="
    call :CheckPort 5433 || set "NEEDADMIN=1"
    call :CheckPort 6379 || set "NEEDADMIN=1"
    if defined NEEDADMIN (
        echo [Sub2API] 需要管理员权限来启动 PostgreSQL / Redis 服务，正在请求提权...
        powershell -NoProfile -Command "Start-Process -Verb RunAs -FilePath '%~f0'"
        exit /b
    )
)

rem ---------- 1) 确保依赖服务在运行 ----------
call :CheckPort 5433
if errorlevel 1 (
    echo [Sub2API] 启动 PostgreSQL 服务...
    net start postgresql-x64-18
)
call :CheckPort 6379
if errorlevel 1 (
    echo [Sub2API] 启动 Redis-Memurai 服务...
    net start Memurai
)

rem ---------- 2) 已在运行？直接打开浏览器 ----------
tasklist /FI "IMAGENAME eq sub2api.exe" 2>nul | findstr /I "sub2api.exe" >nul
if not errorlevel 1 (
    echo [Sub2API] 服务已在运行，打开浏览器...
    start "" "%URL%"
    exit /b 0
)

rem ---------- 3) 隐藏窗口启动 sub2api ----------
echo [Sub2API] 正在以无窗口模式启动服务...
if not exist "%LOGDIR%" mkdir "%LOGDIR%" >nul 2>&1
set "VBS=%TEMP%\sub2api-launcher.vbs"
powershell -NoProfile -Command "([wmiclass]'Win32_Process').Create('cmd /c set CONFIG_FILE=%BASE%backend\config.yaml&& set DATA_DIR=%BASE%backend&& %BASE%backend\sub2api.exe > %LOG% 2>&1')" >nul 2>&1
ping -n 2 127.0.0.1 >nul
tasklist /FI "IMAGENAME eq sub2api.exe" 2>nul | findstr /I "sub2api.exe" >nul
if errorlevel 1 (
    echo [Sub2API] WMI 启动未生效，改用最小化窗口方式启动...
    start "Sub2API" /min /d "%BASE%backend" sub2api.exe
)
rem 清理可能残留的旧临时文件
del "%VBS%" >nul 2>&1

rem ---------- 4) 等待服务就绪，最多约 45 秒 ----------
echo [Sub2API] 等待服务就绪...
call :WaitReady
if errorlevel 1 (
    echo [Sub2API] 启动失败或超时，请查看日志：%LOG%
    timeout /t 15 /nobreak >nul 2>&1
    exit /b 1
)
echo [Sub2API] 启动成功，打开浏览器...
start "" "%URL%"
exit /b 0

rem ---------- 工具函数：等待 8080 就绪 ----------
:WaitReady
for /L %%i in (1,1,45) do (
    curl -s -m 2 -o nul "%URL%/health" >nul 2>&1
    if not errorlevel 1 exit /b 0
    ping -n 2 127.0.0.1 >nul
)
exit /b 1

rem ---------- 工具函数：检测本机端口是否在监听 ----------
:CheckPort
netstat -ano | findstr ":%~1 " | findstr "LISTENING" >nul
exit /b %errorlevel%
