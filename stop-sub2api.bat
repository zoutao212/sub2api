@echo off

rem ============================================================
rem  Sub2API 停止脚本（仅停止 sub2api 本身）
rem  PostgreSQL / Redis 系统服务保持运行，如需停止请手动操作
rem ============================================================

tasklist /FI "IMAGENAME eq sub2api.exe" 2>nul | findstr /I "sub2api.exe" >nul
if errorlevel 1 (
    echo [Sub2API] 服务未在运行。
    ping -n 3 127.0.0.1 >nul
    exit /b 0
)

echo [Sub2API] 正在停止服务...
taskkill /F /IM sub2api.exe >nul 2>&1
echo [Sub2API] 已停止。
ping -n 3 127.0.0.1 >nul
exit /b 0
