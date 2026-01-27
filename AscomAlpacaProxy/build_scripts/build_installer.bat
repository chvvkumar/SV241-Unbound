@echo off
setlocal EnableDelayedExpansion

REM Get script directory
cd /d "%~dp0"
set "SCRIPT_DIR=%CD%"
set "PROXY_ROOT=%SCRIPT_DIR%\.."
set "INSTALLER_TEMPLATE=%SCRIPT_DIR%\installer.iss"
set "TEMP_ISS=%PROXY_ROOT%\build\temp_installer.iss"
set "VERSION_INFO=%PROXY_ROOT%\versioninfo.json"
set "ISCC=C:\Program Files (x86)\Inno Setup 6\ISCC.exe"

echo --- Building SV241 Windows Installer ---

REM 0. Cleanup previous installer
if exist "%PROXY_ROOT%\build\SV241-AscomAlpacaProxy-Setup-*.exe" (
    echo [0/4] Cleaning previous installer...
    del "%PROXY_ROOT%\build\SV241-AscomAlpacaProxy-Setup-*.exe"
)

REM 1. Build EXE first
echo [1/4] Building Executable...
call "%SCRIPT_DIR%\build_exe.bat"
if %ERRORLEVEL% NEQ 0 (
    echo Build failed! Aborting installer creation.
    exit /b 1
)

REM 2. Check Inno Setup
if not exist "%ISCC%" (
    echo Error: Inno Setup Compiler not found at: "%ISCC%"
    echo Please install Inno Setup 6 or adjust the path in this script.
    exit /b 1
)

REM 3. Generate ISS Script
echo [2/4] Generating Installer Script...
if not exist "%INSTALLER_TEMPLATE%" (
    echo Error: Installer template not found at %INSTALLER_TEMPLATE%
    exit /b 1
)

powershell -Command "$json = Get-Content -Raw -Path '%VERSION_INFO%'; $v = ConvertFrom-Json -InputObject $json; $pv = $v.StringFileInfo.ProductVersion; $fvObj = $v.FixedFileInfo.FileVersion; $fv = \"$($fvObj.Major).$($fvObj.Minor).$($fvObj.Patch).$($fvObj.Build)\"; $copy = $v.StringFileInfo.LegalCopyright; $iss = Get-Content -Raw -Path '%INSTALLER_TEMPLATE%'; $iss = $iss.Replace('##VERSION##', $pv).Replace('##FILEVERSION##', $fv).Replace('##COPYRIGHT##', $copy); Set-Content -Path '%TEMP_ISS%' -Value $iss -Encoding UTF8; Write-Host \"Installer Version: $pv ($fv)\""

if %ERRORLEVEL% NEQ 0 (
    echo Error generating temporary installer script.
    exit /b 1
)

REM 4. Run ISCC
echo [3/4] Compiling Installer...
pushd "%PROXY_ROOT%"
"%ISCC%" "%TEMP_ISS%"
popd

REM 5. Cleanup
echo [4/4] Cleanup...
if exist "%TEMP_ISS%" del "%TEMP_ISS%"

echo --- Installer Build Complete ---
endlocal
