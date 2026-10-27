@echo off
REM ============================================================
REM  sub2api - one-click upstream sync
REM
REM  main   = pure mirror of upstream (Wei-Shaw/sub2api)
REM  custom = upstream main + local customizations  <- build/deploy
REM
REM  Run this from the repository root. Safe to re-run: every
REM  step is either a fast-forward or a normal merge.
REM ============================================================
setlocal
cd /d "%~dp0"

echo [1/5] Fetch remotes...
git fetch origin --prune || goto :err
git fetch myfork --prune || goto :err

rem A merge interrupted mid-commit leaves the working tree dirty
rem and makes every later checkout fail with a confusing message.
if exist "%CD%\.git\MERGE_HEAD" (
    echo [FAIL] A previous merge never finished committing.
    echo        Finish it:   git commit --no-edit
    echo        Discard it:  git merge --abort
    exit /b 1
)

echo.
echo [2/5] Fast-forward main to upstream...
git checkout main || goto :err
git merge --ff-only origin/main || goto :err
git push myfork main || goto :err

echo.
echo [3/5] Merge upstream into custom...
git checkout custom || goto :err
git merge --no-edit origin/main
if errorlevel 1 goto :conflict

echo.
echo [4/5] Pushing custom...
git push myfork custom || goto :err

echo.
echo [OK] Sync complete. Nothing to do.
goto :done

:conflict
echo.
echo ============================================================
echo  CONFLICTS - resolve them in your editor, then finish with:
echo.
echo    git add -A
echo    git commit --no-edit
echo    git push myfork custom
echo
echo  main is already up to date and pushed; custom_changes.json
echo  lists which hunks are yours vs upstream's.
echo ============================================================
goto :done

:err
echo.
echo [FAIL] Sync stopped. Nothing was force-pushed and no commit was
echo        discarded. Fix the error above and re-run.
exit /b 1

:done
endlocal
pause
