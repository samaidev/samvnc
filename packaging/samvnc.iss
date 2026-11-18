; samvnc.iss — Inno Setup script for SamVNC (Windows tray + embedded server/tunnel)
; Build: iscc.exe /DMyAppVersion=x.y.z /DOutputBaseFilename=SamVNC-<v>-windows-amd64-setup.exe /DCnMessages=<isl> packaging/samvnc.iss
; All source paths are relative to this file.

#ifndef MyAppVersion
#define MyAppVersion "1.7.0"
#endif
#ifndef OutputBaseFilename
#define OutputBaseFilename "SamVNC-Setup-{#MyAppVersion}"
#endif

[Setup]
AppId={{SAMVNC-2026-SAMAIDEV}
AppName=SamVNC
AppVersion={#MyAppVersion}
AppPublisher=samai.cc
AppPublisherURL=https://samai.cc
DefaultDirName={autopf}\SamVNC
DefaultGroupName=SamVNC
DisableProgramGroupPage=yes
OutputDir=dist
OutputBaseFilename={#OutputBaseFilename}
#ifdef SetupIcon
SetupIconFile={#SetupIcon}
#endif
Compression=lzma2/ultra64
SolidCompression=yes
WizardStyle=modern
UninstallDisplayIcon={app}\samvnc-tray-windows-amd64.exe
ArchitecturesInstallIn64BitMode=x64compatible
ArchitecturesAllowed=x64compatible
SetupLogging=yes

#ifdef CnMessages
[Languages]
Name: "chinesesimp"; MessagesFile: "{#CnMessages}"
Name: "english"; MessagesFile: "compiler:Default.isl"
#else
[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"
#endif

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; GroupDescription: "{cm:AdditionalIcons}"
Name: "autostart"; Description: "开机自动启动 SamVNC / Start SamVNC at login"; GroupDescription: "其他选项 / Additional tasks:"; Flags: unchecked

[Files]
Source: "samvnc-tray-windows-amd64.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "tray_icon.ico"; DestDir: "{app}"; Flags: ignoreversion skipifsourcedoesntexist

[Icons]
Name: "{group}\SamVNC"; Filename: "{app}\samvnc-tray-windows-amd64.exe"
Name: "{group}\Uninstall SamVNC"; Filename: "{uninstallexe}"
Name: "{autodesktop}\SamVNC"; Filename: "{app}\samvnc-tray-windows-amd64.exe"; Tasks: desktopicon

[Registry]
Root: HKCU; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; ValueType: string; \
    ValueName: "SamVNC"; ValueData: """{app}\samvnc-tray-windows-amd64.exe"""; \
    Tasks: autostart; Flags: uninsdeletevalue

[Run]
Filename: "{app}\samvnc-tray-windows-amd64.exe"; Description: "{cm:LaunchProgram,SamVNC}"; Flags: nowait postinstall skipifsilent

[UninstallRun]
Filename: "{cmd}"; Parameters: "/C taskkill /F /IM samvnc-tray-windows-amd64.exe"; Flags: runhidden; RunOnceId: "KillTray"

[UninstallDelete]
Type: filesandordirs; Name: "{app}"
