; xnc.iss - XNC unified installer (spec 2026-09-03-innosetup-installer
; sections 3/8/9.3). One setup.exe serves install/repair/upgrade/uninstall;
; services are disposable derived state - every install path is
; stop -> delete -> recreate with a full declaration each time (never
; sc.exe config / binPath mutation).
;
; Built via installer/build.ps1 (or `make installer VERSION=<ver>`), which
; injects:
;   /DVersion=<x.y.z[-tag]>   product version (same source as agent -ldflags)
;   /DSetupVersion=<w.x.y.z>  numeric-only 4 parts, for VersionInfoVersion
;   /DChannel=stable|dev      channel marker in the uninstall registry entry
;
; Zero credentials at install time (spec G3): argv of both services carries
; no secrets and no server; the agent starts unbound and idles awaiting
; registration until `xnc register` writes binding.json.

#ifndef Version
  #define Version "0.0.0"
#endif
#ifndef SetupVersion
  #define SetupVersion "0.0.0.0"
#endif
#ifndef Channel
  #define Channel "stable"
#endif
// 版本号已带 -dev 后缀时不重复频道后缀（防 XNC-Installer-dev-<v>-dev.exe）。
#if (Channel == "dev") && (Pos("-dev", Version) == 0)
  #define ChannelSuffix "-dev"
#else
  #define ChannelSuffix ""
#endif
// 签名证书指纹（build.ps1 自动传入；空 = 本次构建未签名，跳过信任装卸）
#ifndef CertThumb
  #define CertThumb ""
#endif

[Setup]
; Fixed AppId: upgrades/uninstalls match previous installs (same id = same
; uninstall entry; "upgrade" is just a silent re-run, spec section 9).
AppId={{6F3C2A91-8D4B-4E57-B9A3-0C7E5D82F1A4}
AppName=XNC
AppVersion={#Version}
AppVerName=XNC {#Version}
AppPublisher=XNC
DefaultDirName={autopf}\XNC
AppendDefaultDirName=no
PrivilegesRequired=admin
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0
; Serialize installer runs (update orchestration may race a manual install).
SetupMutex=XNC-Installer
; Same-dir re-runs are the upgrade/repair path, not an accident.
DirExistsWarning=no
; In-use xnc.exe is handled by the prep rename step, not Restart Manager.
CloseApplications=no
DisableProgramGroupPage=yes
UninstallDisplayName=XNC
VersionInfoVersion={#SetupVersion}
OutputDir=..\..\bin
OutputBaseFilename=XNC-Installer{#ChannelSuffix}-{#Version}
Compression=lzma2/max
SolidCompression=yes
WizardStyle=modern

[Types]
Name: "full"; Description: "Full installation"
Name: "custom"; Description: "Custom installation"; Flags: iscustom

[Components]
Name: "agent"; Description: "XNC Agent + CLI (required)"; Types: full custom; Flags: fixed
Name: "desktop"; Description: "XNC Core + Desktop (remote desktop sessions)"; Types: full custom
Name: "shell"; Description: "XNC Shell (ConPTY shell sessions)"; Types: full custom

[Files]
; Five binaries from bin\ (built by installer/build.ps1). Agent + CLI are a
; fixed component (spec G2); core+desktop and shell are croppable (spec 3.2).
Source: "..\..\bin\xnc-agent.exe"; DestDir: "{app}"; Components: agent; Flags: ignoreversion
Source: "..\..\bin\xnc.exe"; DestDir: "{app}"; Components: agent; Flags: ignoreversion restartreplace
Source: "..\..\bin\xnc-core.exe"; DestDir: "{app}"; Components: desktop; Flags: ignoreversion
Source: "..\..\bin\xnc-host.exe"; DestDir: "{app}"; Components: desktop; Flags: ignoreversion
Source: "..\..\bin\xnc-shell.exe"; DestDir: "{app}"; Components: shell; Flags: ignoreversion
; 签名公钥：安装时导入本机信任（自签过渡期的机群信任分发；正式 CA 后移除）
Source: "codesign.cer"; DestDir: "{tmp}"; Flags: ignoreversion

[UninstallDelete]
; Runtime-generated files under {app} that setup never copied and hence is
; not tracking: xnc-core writes its service log next to its exe, and the
; prep step's in-use CLI swap leaves xnc.exe.old behind (also removed by the
; uninstall prep script; this is the belt-and-braces pass).
Type: files; Name: "{app}\*.log"
Type: files; Name: "{app}\*.old"

[Code]
const
  EnvRegKey = 'SYSTEM\CurrentControlSet\Control\Session Manager\Environment';
  UninstallKey = 'Software\Microsoft\Windows\CurrentVersion\Uninstall\{6F3C2A91-8D4B-4E57-B9A3-0C7E5D82F1A4}_is1';
  // Rollback watchdog scheduled task name (spec 9.4; registered by the agent
  // updater, deleted here best-effort on uninstall).
  WatchdogTaskName = 'XNCRollbackWatchdog';
  // 签名证书指纹（ISCC /DCertThumb 注入；空 = 未签名构建，跳过信任装卸）。
  CertThumb = '{#CertThumb}';
  WM_SETTINGCHANGE = $001A;
  // HWND_BROADCAST ($FFFF) is predefined by Inno Setup 6.4+.
  SMTO_ABORTIFHUNG = $0002;

var
  gPurgeData: Boolean;

// WPARAM/LPARAM are not declared types in Inno's script engine; UINT_PTR and
// DWORD_PTR are the pointer-width equivalents (correct on 64-bit installs).
procedure SendMessageTimeout(hWnd: HWND; Msg: UINT; wParam: UINT_PTR;
  lParam: String; fuFlags: UINT; uTimeout: UINT;
  var lpdwResult: DWORD_PTR);
external 'SendMessageTimeoutW@user32.dll stdcall';

// ---- shared helpers ----

// Machine-level state dir (spec 5.2). GetEnv works in both the installer and
// the uninstaller; {commonappdata} is not reliably expandable in uninstall
// code.
function XNCStateDir(): String;
var
  pd: String;
begin
  pd := GetEnv('ProgramData');
  if pd = '' then
    pd := 'C:\ProgramData';
  Result := pd + '\XNC';
end;

function PowerShellExe(): String;
begin
  Result := ExpandConstant('{sys}\WindowsPowerShell\v1.0\powershell.exe');
end;

procedure InstallerLog(Msg: String);
begin
  SaveStringToFile(XNCStateDir() + '\installer.log',
    GetDateTimeString('yyyy/mm/dd hh:nn:ss', '-', ':') + ' ' + Msg + #13#10, True);
end;

// Run a PowerShell script file hidden; returns its exit code, or -1 when the
// interpreter itself could not be started. All scripts are ASCII-only so
// SaveStringToFile (ANSI) and PS 5.1 agree on the encoding. Values reach the
// scripts via baked-in param defaults - never via Mandatory parameters: PS
// ignores defaults on Mandatory params and prompts, which hangs a hidden
// SW_HIDE window forever (session-0 silent installs must never prompt).
function RunPsScript(ScriptPath: String): Integer;
var
  rc: Integer;
begin
  Result := -1;
  if Exec(PowerShellExe(),
      '-NoProfile -ExecutionPolicy Bypass -File "' + ScriptPath + '"', '',
      SW_HIDE, ewWaitUntilTerminated, rc) then
    Result := rc;
end;

// ---- embedded PowerShell ----

// Prep script (spec 9.3 step 2 / spec 8 steps 2-3): stop+delete both services
// (agent first, core second), kill residual desktop/shell processes whose
// image lives under the install dir, drop historical *.old and (install path
// only, RenameCli=1) rename an in-use xnc.exe to xnc.exe.old - Windows allows
// renaming a running exe. Tolerates absent services at first install.
function PrepPsScript(InstallDir, RenameCli, LogFile: String): String;
begin
  Result :=
    'param(' + #13#10 +
    '    [string]$InstallDir = ''@INSTALLDIR@'',' + #13#10 +
    '    [string]$LogFile = ''@LOGFILE@'',' + #13#10 +
    '    [string]$RenameCli = ''@RENAMECLI@''' + #13#10 +
    ')' + #13#10 +
    '$ErrorActionPreference = ''Stop''' + #13#10 +
    'function Log([string]$msg) {' + #13#10 +
    '    "$(Get-Date -Format s) $msg" | Out-File -FilePath $LogFile -Append -Encoding ascii' + #13#10 +
    '}' + #13#10 +
    'function Stop-AndDeleteService([string]$name) {' + #13#10 +
    '    $svc = Get-Service -Name $name -ErrorAction SilentlyContinue' + #13#10 +
    '    if (-not $svc) { Log "prep: $name not present (nothing to do)"; return }' + #13#10 +
    '    if ($svc.Status -ne ''Stopped'') {' + #13#10 +
    '        Log "prep: stopping $name (status $($svc.Status))"' + #13#10 +
    '        Stop-Service -Name $name -Force -ErrorAction SilentlyContinue' + #13#10 +
    '        $deadline = (Get-Date).AddSeconds(20)' + #13#10 +
    '        while ((Get-Date) -lt $deadline) {' + #13#10 +
    '            $svc = Get-Service -Name $name -ErrorAction SilentlyContinue' + #13#10 +
    '            if (-not $svc -or $svc.Status -eq ''Stopped'') { break }' + #13#10 +
    '            Start-Sleep -Milliseconds 500' + #13#10 +
    '        }' + #13#10 +
    '    }' + #13#10 +
    '    # STOP before DELETE: deleting a running service only marks it, which' + #13#10 +
    '    # would make the same-name New-Service below fail (spec 3.2).' + #13#10 +
    '    for ($i = 1; $i -le 3; $i++) {' + #13#10 +
    '        sc.exe delete $name | Out-Null' + #13#10 +
    '        if ($LASTEXITCODE -eq 0) { Log "prep: service $name deleted"; return }' + #13#10 +
    '        Log "prep: sc.exe delete $name exit=$LASTEXITCODE (try $i/3), retrying"' + #13#10 +
    '        Start-Sleep -Seconds 2' + #13#10 +
    '    }' + #13#10 +
    '    throw "prep: sc.exe delete $name still failing (exit $LASTEXITCODE)"' + #13#10 +
    '}' + #13#10 +
    'foreach ($name in @(''XNCAgent'', ''XNCCore'')) { Stop-AndDeleteService $name }' + #13#10 +
    '# Residual session children: kill only processes whose image is under the' + #13#10 +
    '# install dir (mirror-path match; dev machines may run same-named exes' + #13#10 +
    '# from elsewhere).' + #13#10 +
    '$prefix = $InstallDir.TrimEnd(''\'') + ''\''' + #13#10 +
    '$procs = @(Get-CimInstance Win32_Process -Filter ''Name="xnc-host.exe" OR Name="xnc-shell.exe"'' -ErrorAction SilentlyContinue)' + #13#10 +
    'foreach ($p in $procs) {' + #13#10 +
    '    if ($p.ExecutablePath -and $p.ExecutablePath.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)) {' + #13#10 +
    '        Log "prep: killing residual $($p.Name) pid=$($p.ProcessId)"' + #13#10 +
    '        Stop-Process -Id $p.ProcessId -Force -ErrorAction SilentlyContinue' + #13#10 +
    '    }' + #13#10 +
    '}' + #13#10 +
    'Remove-Item -Path (Join-Path $InstallDir ''*.old'') -Force -ErrorAction SilentlyContinue' + #13#10 +
    'if ($RenameCli -eq ''1'') {' + #13#10 +
    '    $cli = Join-Path $InstallDir ''xnc.exe''' + #13#10 +
    '    if (Test-Path $cli) {' + #13#10 +
    '        try {' + #13#10 +
    '            Rename-Item -Path $cli -NewName ''xnc.exe.old'' -Force' + #13#10 +
    '            Log ''prep: renamed xnc.exe -> xnc.exe.old (in-use swap)''' + #13#10 +
    '        } catch {' + #13#10 +
    '            Log "prep: WARN could not rename xnc.exe: $($_.Exception.Message)"' + #13#10 +
    '        }' + #13#10 +
    '    }' + #13#10 +
    '}' + #13#10 +
    'Log ''prep: done''' + #13#10 +
    'exit 0';
  StringChangeEx(Result, '@INSTALLDIR@', InstallDir, True);
  StringChangeEx(Result, '@LOGFILE@', LogFile, True);
  StringChangeEx(Result, '@RENAMECLI@', RenameCli, True);
end;

// Post-install script (spec 3.2 / 9.3 steps 4-5): recreate XNCCore (core
// component only) then XNCAgent with a full declaration every time - New-
// Service, never sc.exe config; embedded quotes in binPath survive PS 5.1
// only via New-Service (see scripts/install-xnccore.ps1). Restart-on-failure
// recovery via sc.exe failure (bare service name, no quoting hazard). Then
// cache the running setup.exe as the Task 7 rollback source (keep exactly
// one) and start the services, tolerating StartPending (poll, never fail
// hard on a slow start).
function PostPsScript(InstallDir, StateDir, WithCore, SetupExe,
  LogFile: String): String;
begin
  Result :=
    'param(' + #13#10 +
    '    [string]$InstallDir = ''@INSTALLDIR@'',' + #13#10 +
    '    [string]$StateDir = ''@STATEDIR@'',' + #13#10 +
    '    [string]$WithCore = ''@WITHCORE@'',' + #13#10 +
    '    [string]$SetupExe = ''@SETUPEXE@'',' + #13#10 +
    '    [string]$LogFile = ''@LOGFILE@''' + #13#10 +
    ')' + #13#10 +
    '$ErrorActionPreference = ''Stop''' + #13#10 +
    'function Log([string]$msg) {' + #13#10 +
    '    "$(Get-Date -Format s) $msg" | Out-File -FilePath $LogFile -Append -Encoding ascii' + #13#10 +
    '}' + #13#10 +
    'New-Item -ItemType Directory -Force -Path $StateDir | Out-Null' + #13#10 +
    'function New-XncService([string]$name, [string]$display, [string]$binPath, [string]$desc) {' + #13#10 +
    '    for ($i = 1; $i -le 4; $i++) {' + #13#10 +
    '        try {' + #13#10 +
    '            New-Service -Name $name -DisplayName $display -BinaryPathName $binPath -StartupType Automatic -Description $desc | Out-Null' + #13#10 +
    '            return $true' + #13#10 +
    '        } catch {' + #13#10 +
    '            Log "post: New-Service $name failed (try $i/4): $($_.Exception.Message)"' + #13#10 +
    '            Start-Sleep -Seconds 2' + #13#10 +
    '        }' + #13#10 +
    '    }' + #13#10 +
    '    return $false' + #13#10 +
    '}' + #13#10 +
    'function Set-Recovery([string]$name) {' + #13#10 +
    '    sc.exe failure $name reset= 86400 actions= restart/5000/restart/5000/restart/5000 | Out-Null' + #13#10 +
    '    if ($LASTEXITCODE -ne 0) { Log "post: WARN sc.exe failure $name exit=$LASTEXITCODE" }' + #13#10 +
    '}' + #13#10 +
    'function Start-AndPoll([string]$name) {' + #13#10 +
    '    $svc = Get-Service -Name $name -ErrorAction SilentlyContinue' + #13#10 +
    '    if (-not $svc) { Log "post: WARN service $name missing"; return }' + #13#10 +
    '    if ($svc.Status -ne ''Running'') { Start-Service -Name $name -ErrorAction SilentlyContinue }' + #13#10 +
    '    $deadline = (Get-Date).AddSeconds(30)' + #13#10 +
    '    do {' + #13#10 +
    '        $svc = Get-Service -Name $name -ErrorAction SilentlyContinue' + #13#10 +
    '        if (-not $svc) { Log "post: WARN service $name vanished"; return }' + #13#10 +
    '        if ($svc.Status -eq ''Running'') { Log "post: service $name Running"; return }' + #13#10 +
    '        Start-Sleep -Milliseconds 500' + #13#10 +
    '    } while ((Get-Date) -lt $deadline)' + #13#10 +
    '    Log "post: WARN service $name still $($svc.Status) after 30s (StartPending tolerated)"' + #13#10 +
    '}' + #13#10 +
    'if ($WithCore -eq ''1'') {' + #13#10 +
    '    $coreExe = Join-Path $InstallDir ''xnc-core.exe''' + #13#10 +
    '    $secret = Join-Path $StateDir ''core-secret.hex''' + #13#10 +
    '    $binPath = ''"'' + $coreExe + ''" --service XNCCore --secret-file "'' + $secret + ''"''' + #13#10 +
    '    if (-not (New-XncService ''XNCCore'' ''XNC Node Core'' $binPath ''XNC node core (XNIP console pipe server)'')) {' + #13#10 +
    '        Log ''post: FATAL XNCCore creation failed''' + #13#10 +
    '        exit 1' + #13#10 +
    '    }' + #13#10 +
    '    Set-Recovery ''XNCCore''' + #13#10 +
    '}' + #13#10 +
    '# --server= satisfies the run subcommand''s required flag while leaving' + #13#10 +
    '# the agent unbound: no binding.json -> unregistered idle (spec 6.1);' + #13#10 +
    '# binding.json (written by xnc register) is the server source from then' + #13#10 +
    '# on. Zero secrets in argv (spec 3.2).' + #13#10 +
    '$agentExe = Join-Path $InstallDir ''xnc-agent.exe''' + #13#10 +
    '$binPath = ''"'' + $agentExe + ''" run --server= --state-dir="'' + $StateDir + ''"''' + #13#10 +
    'if (-not (New-XncService ''XNCAgent'' ''XNC Agent'' $binPath ''XNC node agent (outbound control connection)'')) {' + #13#10 +
    '    Log ''post: FATAL XNCAgent creation failed''' + #13#10 +
    '    exit 1' + #13#10 +
    '}' + #13#10 +
    'Set-Recovery ''XNCAgent''' + #13#10 +
    '# installer-cache: the running setup.exe is the rollback source for the' + #13#10 +
    '# update orchestration (spec 9.2); keep exactly one copy. The rollback' + #13#10 +
    '# path (spec 9.4) executes the cached exe IN PLACE, so the cache refresh' + #13#10 +
    '# must tolerate source == destination: Copy-Item onto itself throws under' + #13#10 +
    '# Stop-preference (T6-T7 contract: rollback runs the cache entry directly,' + #13#10 +
    '# no copy-to-staging first; staging is download-only, spec 9.2).' + #13#10 +
    '$cacheDir = Join-Path $StateDir ''installer-cache''' + #13#10 +
    'New-Item -ItemType Directory -Force -Path $cacheDir | Out-Null' + #13#10 +
    '$leaf = Split-Path -Leaf $SetupExe' + #13#10 +
    '$dest = Join-Path $cacheDir $leaf' + #13#10 +
    '$samePath = [string]::Equals([System.IO.Path]::GetFullPath($SetupExe), [System.IO.Path]::GetFullPath($dest), [System.StringComparison]::OrdinalIgnoreCase)' + #13#10 +
    'if ($samePath) {' + #13#10 +
    '    Log "post: setup running from installer-cache in place ($leaf); cache entry already current"' + #13#10 +
    '} else {' + #13#10 +
    '    Copy-Item -Path $SetupExe -Destination $dest -Force' + #13#10 +
    '    Log "post: installer-cache refreshed with $leaf"' + #13#10 +
    '}' + #13#10 +
    'Get-ChildItem -Path $cacheDir -Filter ''*.exe'' | Where-Object { $_.Name -ne $leaf } | Remove-Item -Force' + #13#10 +
    'Log "post: installer-cache holds $leaf"' + #13#10 +
    'if ($WithCore -eq ''1'') { Start-AndPoll ''XNCCore'' }' + #13#10 +
    'Start-AndPoll ''XNCAgent''' + #13#10 +
    'Log ''post: done''' + #13#10 +
    'exit 0';
  StringChangeEx(Result, '@INSTALLDIR@', InstallDir, True);
  StringChangeEx(Result, '@STATEDIR@', StateDir, True);
  StringChangeEx(Result, '@WITHCORE@', WithCore, True);
  StringChangeEx(Result, '@SETUPEXE@', SetupExe, True);
  StringChangeEx(Result, '@LOGFILE@', LogFile, True);
end;

// ---- HKLM PATH (spec 3.2) ----

procedure BroadcastEnvironmentChange();
var
  res: DWORD_PTR;
begin
  SendMessageTimeout(HWND_BROADCAST, WM_SETTINGCHANGE, 0, 'Environment',
    SMTO_ABORTIFHUNG, 5000, res);
end;

function ReadSystemPath(): String;
begin
  if not RegQueryStringValue(HKEY_LOCAL_MACHINE, EnvRegKey, 'Path', Result) then
    Result := '';
end;

// REG_EXPAND_SZ keeps %SystemRoot%-style entries expandable.
procedure WriteSystemPath(NewPath: String);
begin
  RegWriteExpandStringValue(HKEY_LOCAL_MACHINE, EnvRegKey, 'Path', NewPath);
end;

function PathEntriesContain(Path, Dir: String): Boolean;
var
  rest, entry: String;
  p: Integer;
begin
  Result := False;
  rest := Path;
  while rest <> '' do
  begin
    p := Pos(';', rest);
    if p > 0 then
    begin
      entry := Copy(rest, 1, p - 1);
      Delete(rest, 1, p);
    end
    else
    begin
      entry := rest;
      rest := '';
    end;
    if Lowercase(Trim(entry)) = Lowercase(Dir) then
    begin
      Result := True;
      Exit;
    end;
  end;
end;

// Idempotent append of {app} to HKLM Environment\Path + WM_SETTINGCHANGE.
procedure AddAppDirToPath();
var
  path, appDir: String;
begin
  appDir := ExpandConstant('{app}');
  path := ReadSystemPath();
  if PathEntriesContain(path, appDir) then
    Exit;
  if path = '' then
    path := appDir
  else
    path := path + ';' + appDir;
  WriteSystemPath(path);
  BroadcastEnvironmentChange();
end;

procedure RemoveAppDirFromPath();
var
  path, appDir, rest, entry, kept: String;
  p: Integer;
  first: Boolean;
begin
  appDir := ExpandConstant('{app}');
  path := ReadSystemPath();
  if not PathEntriesContain(path, appDir) then
    Exit;
  rest := path;
  kept := '';
  first := True;
  while rest <> '' do
  begin
    p := Pos(';', rest);
    if p > 0 then
    begin
      entry := Copy(rest, 1, p - 1);
      Delete(rest, 1, p);
    end
    else
    begin
      entry := rest;
      rest := '';
    end;
    if Lowercase(Trim(entry)) <> Lowercase(appDir) then
    begin
      if not first then
        kept := kept + ';';
      kept := kept + entry;
      first := False;
    end;
  end;
  WriteSystemPath(kept);
  BroadcastEnvironmentChange();
end;

// ---- install flow ----

// Spec 9.3 step 2: runs for first install and upgrades alike (services are
// disposable; there is deliberately no "already installed?" branch).
function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  script: String;
  rc: Integer;
begin
  Result := '';
  ForceDirectories(XNCStateDir());
  script := ExpandConstant('{tmp}') + '\xnc-prep.ps1';
  if not SaveStringToFile(script,
      PrepPsScript(ExpandConstant('{app}'), '1',
        XNCStateDir() + '\installer.log'), False) then
  begin
    Result := 'Failed to write the XNC prepare script.';
    Exit;
  end;
  rc := RunPsScript(script);
  if rc <> 0 then
    Result := 'XNC prepare step failed (exit code ' + IntToStr(rc) +
      '). See ' + XNCStateDir() + '\installer.log';
end;

// Spec 3.2/9.3 steps 4-5: recreate services (full declaration), cache the
// running installer, start services, then PATH. ANY nonzero script exit is
// fatal to the install (raises -> nonzero setup exit code, which the update
// orchestration keys on, spec 9.3/9.4): that includes RunPsScript's -1
// (powershell.exe could not be launched at all) and any unexpected PS
// failure after services were created - silently succeeding with no
// services/cache is the worst outcome. A service merely slow to reach
// Running stays a warning (the script itself exits 0 in that case).
// InstallSignTrust: 自签证书入本机 Root + TrustedPublisher（certutil 免 PS
// 依赖）。失败仅告警不中断——信任缺失的后果是 Defender 可能隔离，不该让
// 安装失败；正式 CA 落地后本步骤整体退役。
procedure InstallSignTrust();
var
  rc: Integer;
begin
  if CertThumb = '' then
    Exit;
  if not Exec(ExpandConstant('{sys}\certutil.exe'),
      '-addstore Root "' + ExpandConstant('{tmp}\codesign.cer') + '"', '',
      SW_HIDE, ewWaitUntilTerminated, rc) or (rc <> 0) then
    InstallerLog('WARN: certutil addstore Root rc=' + IntToStr(rc) +
      ' (Defender may quarantine unsigned-trust binaries)');
  if not Exec(ExpandConstant('{sys}\certutil.exe'),
      '-addstore TrustedPublisher "' + ExpandConstant('{tmp}\codesign.cer') +
      '"', '', SW_HIDE, ewWaitUntilTerminated, rc) or (rc <> 0) then
    InstallerLog('WARN: certutil addstore TrustedPublisher rc=' + IntToStr(rc));
end;

procedure CurStepChanged(CurStep: TSetupStep);
var
  script, withCore: String;
  rc: Integer;
begin
  if CurStep <> ssPostInstall then
    Exit;
  InstallSignTrust();
  if WizardIsComponentSelected('desktop') then
    withCore := '1'
  else
    withCore := '0';
  ForceDirectories(XNCStateDir());
  script := ExpandConstant('{tmp}') + '\xnc-post.ps1';
  if not SaveStringToFile(script,
      PostPsScript(ExpandConstant('{app}'), XNCStateDir(), withCore,
        ExpandConstant('{srcexe}'), XNCStateDir() + '\installer.log'), False) then
    RaiseException('Failed to write the XNC service script.');
  rc := RunPsScript(script);
  if rc <> 0 then
    RaiseException('XNC service registration failed (script exit code ' +
      IntToStr(rc) + '). See ' + XNCStateDir() + '\installer.log');
  AddAppDirToPath();
  // Channel marker in the uninstall entry (spec 3.2; DisplayVersion and
  // InstallDate are written by Inno itself). Deliberately NOT a [Registry]
  // entry: that section runs before Inno saves uninstall info, and
  // RegisterUninstallInfo deletes any pre-existing _is1 key ("leftover from
  // previous install") before recreating it - wiping values written there
  // moments earlier. ssPostInstall runs after the key is final. The whole
  // key is deleted at uninstall, so no uninsdeletevalue bookkeeping.
  if not RegWriteStringValue(HKEY_LOCAL_MACHINE, UninstallKey, 'Channel',
      '{#Channel}') then
    InstallerLog('WARN: could not write Channel marker to uninstall key');
end;

// ---- uninstall flow (spec 8) ----

// /PURGEDATA[=true|1] forces data deletion in silent uninstalls (spec 8
// step 6); =false/=0 disables.
function PurgeDataRequested(): Boolean;
var
  tail, rest: String;
  p: Integer;
begin
  Result := False;
  tail := Uppercase(GetCmdTail());
  p := Pos('/PURGEDATA', tail);
  if p = 0 then
    Exit;
  rest := Copy(tail, p + Length('/PURGEDATA'), Length(tail));
  if Copy(rest, 1, 6) = '=FALSE' then
    Exit;
  if Copy(rest, 1, 2) = '=0' then
    Exit;
  Result := True;
end;

// Interactive data question (spec 8 step 5). Also carries the deregister
// notice from the spec: the uninstaller never deregisters the node, so the
// user must run `xnc deregister` first if server-side removal is wanted -
// hence the question is asked BEFORE files (incl. xnc.exe) are deleted.
// Returns 0 = keep data, 1 = purge data, -1 = cancelled (the intro tells the
// user to run `xnc deregister` BEFORE continuing; Cancel must abort the
// uninstall, not silently proceed with it).
function AskDataPurge(): Integer;
var
  Form: TSetupForm;
  Intro: TLabel;
  Keep, Purge: TRadioButton;
  OKBtn, CancelBtn: TButton;
begin
  Result := -1;
  // Inno 6.3+ signature: CreateCustomForm(ClientWidth, ClientHeight,
  // KeepSizeX, KeepSizeY) - fixed dialog, no user resizing.
  Form := CreateCustomForm(ScaleX(440), ScaleY(208), True, True);
  try
    Form.Caption := 'XNC node data';
    Intro := TLabel.Create(Form);
    Intro.Parent := Form;
    Intro.Left := ScaleX(16);
    Intro.Top := ScaleY(12);
    Intro.Width := Form.ClientWidth - ScaleX(32);
    Intro.WordWrap := True;
    Intro.Caption :=
      'The node remains registered on the XNC server - uninstalling does ' +
      'not deregister it. To remove it from the server, run ' +
      '"xnc deregister" BEFORE continuing.' + #13#10#13#10 +
      'Keep the machine-level state in ' + XNCStateDir() + ' (node ' +
      'identity, binding, logs)? Reinstalling reuses the identity.';
    Keep := TRadioButton.Create(Form);
    Keep.Parent := Form;
    Keep.Left := ScaleX(16);
    Keep.Top := ScaleY(104);
    Keep.Width := Form.ClientWidth - ScaleX(32);
    Keep.Caption := 'Keep ' + XNCStateDir() + ' (recommended)';
    Keep.Checked := True;
    Purge := TRadioButton.Create(Form);
    Purge.Parent := Form;
    Purge.Left := ScaleX(16);
    Purge.Top := ScaleY(130);
    Purge.Width := Form.ClientWidth - ScaleX(32);
    Purge.Caption := 'Delete ' + XNCStateDir() + ' (including logs)';
    OKBtn := TButton.Create(Form);
    OKBtn.Parent := Form;
    OKBtn.Caption := 'OK';
    OKBtn.ModalResult := mrOk;
    OKBtn.Left := Form.ClientWidth - ScaleX(186);
    OKBtn.Top := ScaleY(168);
    OKBtn.Width := ScaleX(80);
    CancelBtn := TButton.Create(Form);
    CancelBtn.Parent := Form;
    CancelBtn.Caption := 'Cancel';
    CancelBtn.ModalResult := mrCancel;
    CancelBtn.Left := Form.ClientWidth - ScaleX(96);
    CancelBtn.Top := ScaleY(168);
    CancelBtn.Width := ScaleX(80);
    if Form.ShowModal() = mrOk then
      if Purge.Checked then
        Result := 1
      else
        Result := 0;
  finally
    Form.Free();
  end;
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
var
  script: String;
  rc: Integer;
begin
  if CurUninstallStep = usUninstall then
  begin
    // Step 5 decision first (see AskDataPurge); silent uninstalls default
    // to KEEP unless /PURGEDATA (spec 8 step 6). Interactive Cancel aborts
    // the whole uninstall so the user can run `xnc deregister` first.
    if UninstallSilent() then
    begin
      if PurgeDataRequested() then
        gPurgeData := True
    end
    else
    begin
      case AskDataPurge() of
        1: gPurgeData := True;
        0: gPurgeData := False;
      else
        RaiseException('Uninstall cancelled. Run "xnc deregister" first if ' +
          'you want the node removed from the XNC server, then uninstall again.');
      end;
    end;
    // Step 1: cancel any in-flight/pending update machinery.
    Exec(ExpandConstant('{sys}\schtasks.exe'),
      '/Delete /TN ' + WatchdogTaskName + ' /F', '', SW_HIDE,
      ewWaitUntilTerminated, rc);
    DeleteFile(XNCStateDir() + '\update-pending.json');
    DelTree(XNCStateDir() + '\installer-cache', True, True, True);
    DelTree(XNCStateDir() + '\staging', True, True, True);
    // Steps 2-3: stop+delete services (agent -> core), kill residual
    // desktop/shell processes, drop *.old (prep script, no CLI rename).
    script := GetTempDir() + '\xnc-uninstall.ps1';
    if SaveStringToFile(script,
        PrepPsScript(ExpandConstant('{app}'), '0',
          XNCStateDir() + '\installer.log'), False) then
    begin
      rc := RunPsScript(script);
      if rc <> 0 then
      begin
        // Best-effort on purpose (spec 8 tolerance): a service that will not
        // delete must not stop us removing what CAN be removed (PATH,
        // registry, unlocked files) - aborting here would leave strictly more
        // residue. The failure is not silent: if a service is still RUNNING
        // its exe is locked and Inno's own file-deletion step fails the
        // uninstall (nonzero exit); a stopped-but-undeletable (marked-for-
        // deletion) service leaves a stale SCM entry until reboot, which this
        // warning names, and re-running the uninstall usually clears.
        InstallerLog('WARN: uninstall prep script exit ' + IntToStr(rc) +
          ' (see installer.log; a stale SCM entry may remain until reboot)');
        if not UninstallSilent() then
          MsgBox('XNC uninstall could not fully clean up the XNCCore/XNCAgent ' +
            'services (exit code ' + IntToStr(rc) + '). A stale service entry ' +
            'may remain until the machine reboots; re-running the uninstall ' +
            'usually clears it. See ' + XNCStateDir() + '\installer.log',
            mbError, MB_OK);
      end;
      DeleteFile(script);
    end;
    // Step 4: {app} files and the uninstall registry entry are removed by
    // Inno itself right after this step.
  end;
  if CurUninstallStep = usPostUninstall then
  begin
    RemoveAppDirFromPath();
    // 移除签名信任（按指纹精确删；若有更新版本已装会重写——重装/升级都会
    // 重新 addstore）。失败仅告警。
    if CertThumb <> '' then
    begin
      if not Exec(ExpandConstant('{sys}\certutil.exe'),
          '-delstore Root ' + CertThumb, '', SW_HIDE,
          ewWaitUntilTerminated, rc) then
        InstallerLog('WARN: certutil delstore Root failed');
      if not Exec(ExpandConstant('{sys}\certutil.exe'),
          '-delstore TrustedPublisher ' + CertThumb, '', SW_HIDE,
          ewWaitUntilTerminated, rc) then
        InstallerLog('WARN: certutil delstore TrustedPublisher failed');
    end;
    if gPurgeData then
      DelTree(XNCStateDir(), True, True, True);
  end;
end;
