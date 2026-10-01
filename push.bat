@echo off
REM Push the master server's commits to GitHub (Predator04/soldat-master-server).
cd /d "%~dp0"
git push origin HEAD:master || goto :fail
echo.
echo Done: master server code pushed.
pause
exit /b 0
:fail
echo.
echo Something failed - see the message above.
pause
exit /b 1
