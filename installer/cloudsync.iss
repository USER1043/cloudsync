; Inno Setup script for cloudsync-setup.exe (built by .github/workflows/release.yml).
;
; Per-user install, no administrator rights: the program goes where
; `cloudsync start` has always put it, %LocalAppData%\Programs\CloudSync, and
; that folder is added to the user's PATH so plain `cloudsync ...` works in a
; new terminal. Run from the repo root:
;   ISCC /DVersion=1.2.3 installer\cloudsync.iss
; after putting cloudsync.exe and cloudsync-background.exe in dist\ (the
; second is built with -ldflags "-H=windowsgui", so it opens no console
; window when it runs at login).

#ifndef Version
  #define Version "0.0.0"
#endif

[Setup]
AppId={{02AB9DF2-A41D-4246-884B-0DE7754CE866}
AppName=CloudSync
AppVersion={#Version}
AppPublisher=CloudSync
DefaultDirName={localappdata}\Programs\CloudSync
; The program looks for itself at this fixed place, so don't offer another.
DisableDirPage=yes
DisableProgramGroupPage=yes
PrivilegesRequired=lowest
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
OutputDir=..\dist
OutputBaseFilename=cloudsync-setup
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
ChangesEnvironment=yes
UninstallDisplayName=CloudSync
SetupLogging=yes

[Files]
Source: "..\dist\cloudsync.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\dist\cloudsync-background.exe"; DestDir: "{app}"; Flags: ignoreversion

[Registry]
Root: HKCU; Subkey: "Environment"; ValueType: expandsz; ValueName: "Path"; \
  ValueData: "{olddata};{app}"; Check: NeedsAddPath('{app}')

[Icons]
; Brings the tray icon (and the sync it manages) back after Exit. A second
; copy exits at once: the tray holds a lock.
Name: "{userprograms}\CloudSync"; Filename: "{app}\cloudsync-background.exe"; Parameters: "tray"; Comment: "Show the CloudSync tray icon and start syncing"

[Run]
; An update that replaced a running CloudSync starts it again.
Filename: "{app}\cloudsync.exe"; Parameters: "start"; Flags: runhidden; Check: WasSetToRun
; A fresh install only offers it: `start` needs the sign-in and first sync done.
Filename: "{app}\cloudsync.exe"; Parameters: "start"; \
  Description: "Start CloudSync in the background (only after cloudsync login, init and the first sync)"; \
  Flags: postinstall nowait skipifsilent unchecked; Check: not WasSetToRun

[UninstallRun]
Filename: "{app}\cloudsync.exe"; Parameters: "stop"; RunOnceId: "StopCloudSync"; Flags: runhidden

[UninstallDelete]
Type: files; Name: "{app}\*.previous"
Type: files; Name: "{app}\*.tmp"

[Code]
const
  RunKey = 'Software\Microsoft\Windows\CurrentVersion\Run';

var
  SetToRun: Boolean;

function WasSetToRun: Boolean;
begin
  Result := SetToRun;
end;

// Reports whether Dir is missing from the user's PATH.
function NeedsAddPath(Param: string): Boolean;
var
  Paths: string;
begin
  if not RegQueryStringValue(HKCU, 'Environment', 'Path', Paths) then
    Paths := '';
  Result := Pos(';' + Lowercase(ExpandConstant(Param)) + ';', ';' + Lowercase(Paths) + ';') = 0;
end;

procedure RemoveFromPath(Dir: string);
var
  Paths: string;
  P: Integer;
begin
  if not RegQueryStringValue(HKCU, 'Environment', 'Path', Paths) then
    Exit;
  P := Pos(';' + Lowercase(Dir) + ';', ';' + Lowercase(Paths) + ';');
  if P = 0 then
    Exit;
  Delete(Paths, P, Length(Dir) + 1);
  RegWriteExpandStringValue(HKCU, 'Environment', 'Path', Paths);
end;

// A running CloudSync holds its program files, so stop it first; it uploads
// its last changes before it exits.
function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  Exe: string;
  Code: Integer;
begin
  Result := '';
  SetToRun := RegValueExists(HKCU, RunKey, 'CloudSync');
  Exe := ExpandConstant('{app}\cloudsync.exe');
  if FileExists(Exe) then
  begin
    if not Exec(Exe, 'stop', '', SW_HIDE, ewWaitUntilTerminated, Code) or (Code <> 0) then
      Result := 'CloudSync is still running (maybe in a terminal). Stop it there with Ctrl-C, then run setup again.';
  end;
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
begin
  if CurUninstallStep = usPostUninstall then
    RemoveFromPath(ExpandConstant('{app}'));
end;
