@echo off
REM ---------------------------------------------------------------------------
REM Local one-click build for Windows.
REM Thin wrapper only: the real logic lives in build-local.sh (bash).
REM Requires: Git for Windows (provides bash) + Go 1.25+ on PATH.
REM
REM Usage:
REM   double-click this file
REM   build-local.bat              full build (build + vet + test + exe)
REM   build-local.bat --fast       skip tests
REM   set NOWAIT=1                 do not pause at the end
REM
REM !! Keep this file ASCII-only and CRLF-terminated !!
REM   cmd.exe parses .bat with the OEM code page (GBK on zh-CN). UTF-8 Chinese
REM   comments plus LF-only endings make cmd split lines mid-character and try
REM   to run fragments of the comments as commands ("xxx is not recognized").
REM ---------------------------------------------------------------------------

setlocal
chcp 65001 >nul

REM Prefer a real Git for Windows bash. `where bash` is only a fallback: on many
REM machines it resolves to the WindowsApps stub that launches WSL instead.
set "BASH="
if exist "%ProgramFiles%\Git\bin\bash.exe" set "BASH=%ProgramFiles%\Git\bin\bash.exe"
if not defined BASH if exist "%ProgramFiles(x86)%\Git\bin\bash.exe" set "BASH=%ProgramFiles(x86)%\Git\bin\bash.exe"
if not defined BASH if exist "%LOCALAPPDATA%\Programs\Git\bin\bash.exe" set "BASH=%LOCALAPPDATA%\Programs\Git\bin\bash.exe"
if not defined BASH for /f "delims=" %%i in ('where bash 2^>nul') do if not defined BASH set "BASH=%%i"

if not defined BASH (
  echo [ERROR] bash not found.
  echo         Install Git for Windows, or run build-local.sh from Git Bash directly.
  if not defined NOWAIT pause
  exit /b 1
)

REM %~dp0 ends with a backslash; bash needs forward slashes.
set "HERE=%~dp0"
set "HERE=%HERE:\=/%"

echo Using bash: %BASH%
echo.
"%BASH%" "%HERE%build-local.sh" %*
set "RC=%ERRORLEVEL%"

echo.
if not "%RC%"=="0" echo [ERROR] build failed, exit code %RC%
if not defined NOWAIT pause
exit /b %RC%
