; Inno Setup script for SV241 Ascom Alpaca Proxy
; SEE THE DOCUMENTATION FOR DETAILS ON CREATING INNO SETUP SCRIPT FILES!

#define MyAppName "SV241 Ascom Alpaca Proxy"
#define MyAppPublisher "SV241-Unbound Team"
#define MyAppURL "https://github.com/SV241-Unbound/SV241-Unbound"
#define MyAppExeName "AscomAlpacaProxy.exe"
#define MyAppVersion "##VERSION##"

[Setup]
AppId={{F4F3A3A4-8E44-4B9A-9A8C-5A3D3E2F5B3A}
AppName={#MyAppName}
AppVersion={#MyAppVersion}
AppPublisher={#MyAppPublisher}
AppPublisherURL={#MyAppURL}
AppSupportURL={#MyAppURL}
AppUpdatesURL={#MyAppURL}
DefaultDirName={autopf}\{#MyAppName}
DefaultGroupName=SV241-Unbound
OutputBaseFilename=SV241-AscomAlpacaProxy-Setup-{#MyAppVersion}
OutputDir=.
Compression=lzma
VersionInfoVersion=##FILEVERSION##
VersionInfoCopyright=##COPYRIGHT##
SolidCompression=yes
WizardStyle=modern
ArchitecturesInstallIn64BitMode=x64compatible
UninstallDisplayIcon={app}\{#MyAppExeName}
SetupIconFile=..\icon.ico

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"; Flags: checkablealone
Name: "autostart"; Description: "Start &automatically with Windows"; GroupDescription: "Other:"; Flags: checkablealone

[Dirs]
; Creates the Helper directory in the installation folder
Name: "{app}\Helper"

[Files]
Source: "{#MyAppExeName}"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\icon.ico"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\Helper\Create-Driver.bat"; DestDir: "{app}\Helper"; Flags: ignoreversion
Source: "..\Helper\Create-AscomDriver.ps1"; DestDir: "{app}\Helper"; Flags: ignoreversion
; NOTE: Don't use "Flags: ignoreversion" on any shared system files

[Icons]
Name: "{group}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"
Name: "{autodesktop}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"; Tasks: desktopicon
Name: "{group}\Create SV241 Ascom Driver"; Filename: "{app}\Helper\Create-Driver.bat"; WorkingDir: "{app}\Helper"

[Registry]
; Autostart via Registry - controlled by the 'autostart' task
; 1. Add new System-wide (HKLM) entry
Root: HKLM; Subkey: "SOFTWARE\Microsoft\Windows\CurrentVersion\Run"; \
    ValueType: string; ValueName: "{#MyAppName}"; \
    ValueData: """{app}\{#MyAppExeName}"""; \
    Tasks: autostart; Flags: uninsdeletevalue

[Run]
Filename: "{app}\{#MyAppExeName}"; Description: "{cm:LaunchProgram,{#StringChange(MyAppName, '&', '&&')}}"; Flags: nowait postinstall skipifsilent

[Code]
// Helper function to uninstall previous version
function GetUninstallString(): String;
var
  sUnInstPath: String;
  sUnInstPathKey: String;
begin
  sUnInstPath := '';
  // Hardcoded AppId to ensure we match the registry key exactly (Inno uses single braces in Reg)
  sUnInstPathKey := 'Software\Microsoft\Windows\CurrentVersion\Uninstall\{F4F3A3A4-8E44-4B9A-9A8C-5A3D3E2F5B3A}_is1';

  // Check 32-bit Registry (WOW6432Node) - For previous 32-bit installs
  if RegQueryStringValue(HKLM32, sUnInstPathKey, 'UninstallString', sUnInstPath) then
  begin
    Result := sUnInstPath;
  end
  else
  // Check 64-bit Registry - For previous 64-bit installs
  if RegQueryStringValue(HKLM64, sUnInstPathKey, 'UninstallString', sUnInstPath) then
  begin
    Result := sUnInstPath;
  end
  else
  // Check Current User (HKCU) - In case a Per-User install exists
  if RegQueryStringValue(HKCU32, sUnInstPathKey, 'UninstallString', sUnInstPath) then
  begin
    Result := sUnInstPath;
  end
  else
  if RegQueryStringValue(HKCU64, sUnInstPathKey, 'UninstallString', sUnInstPath) then
  begin
      Result := sUnInstPath;
  end;
end;

function InitializeSetup(): Boolean;
var
  sUnInstallString: String;
  iResultCode: Integer;
begin
  Result := True;
  sUnInstallString := GetUninstallString();
  if sUnInstallString <> '' then
  begin
    sUnInstallString := RemoveQuotes(sUnInstallString);
    if Exec(sUnInstallString, '/SILENT /NORESTART /SUPPRESSMSGBOXES', '', SW_HIDE, ewWaitUntilTerminated, iResultCode) then
    begin
        // Successfully uninstalled
    end
    else
    begin
        MsgBox('Failed to uninstall previous version. Please uninstall manually.', mbError, MB_OK);
        Result := False;
    end;
  end;

  // 2. Force cleanup of legacy HKCU Autostart key (silently) to prevent double-start
  // This is done via code to avoid the Inno Setup "PrivilegesRequired" static warning
  if RegKeyExists(HKCU, 'SOFTWARE\Microsoft\Windows\CurrentVersion\Run') then
  begin
    RegDeleteValue(HKCU, 'SOFTWARE\Microsoft\Windows\CurrentVersion\Run', '{#emit SetupSetting("AppName")}');
  end;
end;