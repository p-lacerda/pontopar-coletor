#ifndef MyAppVersion
  #define MyAppVersion "dev"
#endif
#ifndef SourceExe
  #define SourceExe "..\dist\pontopar-coletor.exe"
#endif
#ifndef SetupOutputBase
  #define SetupOutputBase "PontoPar-Setup"
#endif

[Setup]
AppId={{02E6C03E-6C33-48EF-A2EA-5EF5072D1423}
AppName=PontoPar Coletor
AppVersion={#MyAppVersion}
AppPublisher=PontoPar
DefaultDirName={commonappdata}\PontoParColetor
DisableDirPage=yes
DisableProgramGroupPage=yes
PrivilegesRequired=admin
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
WizardStyle=modern
WizardSizePercent=110
SetupLogging=yes
CloseApplications=yes
RestartApplications=no
OutputDir=..\dist
OutputBaseFilename={#SetupOutputBase}
Compression=lzma2
SolidCompression=yes
UninstallDisplayIcon={app}\pontopar-coletor.exe

[Files]
Source: "{#SourceExe}"; DestDir: "{app}"; DestName: "pontopar-coletor.exe"; Flags: ignoreversion

[Icons]
Name: "{autoprograms}\PontoPar Coletor"; Filename: "{app}\pontopar-coletor.exe"; WorkingDir: "{app}"
Name: "{autodesktop}\PontoPar Coletor"; Filename: "{app}\pontopar-coletor.exe"; WorkingDir: "{app}"; Tasks: desktopicon

[Tasks]
Name: "desktopicon"; Description: "Criar atalho na Área de Trabalho"; GroupDescription: "Atalhos:"; Flags: checkedonce

[Run]
Filename: "{app}\pontopar-coletor.exe"; Parameters: "install-start"; StatusMsg: "Instalando e iniciando o serviço PontoPar..."; Flags: runhidden waituntilterminated
Filename: "{app}\pontopar-coletor.exe"; Description: "Abrir PontoPar Coletor"; Flags: nowait postinstall skipifsilent

[UninstallRun]
Filename: "{app}\pontopar-coletor.exe"; Parameters: "stop"; Flags: runhidden waituntilterminated; RunOnceId: "StopService"
Filename: "{app}\pontopar-coletor.exe"; Parameters: "uninstall"; Flags: runhidden waituntilterminated; RunOnceId: "RemoveService"

[Code]
var
  DevicePage: TInputQueryWizardPage;
  TheraPage: TInputQueryWizardPage;

function ReadJsonValue(Text, Key: String): String;
var
  P, StartPos, EndPos: Integer;
  Quoted: Boolean;
begin
  Result := '';
  P := Pos('"' + Key + '"', Text);
  if P = 0 then exit;
  P := P + Length(Key) + 2;
  while (P <= Length(Text)) and (Text[P] <> ':') do P := P + 1;
  if P > Length(Text) then exit;
  P := P + 1;
  while (P <= Length(Text)) and (Text[P] <= ' ') do P := P + 1;
  Quoted := (P <= Length(Text)) and (Text[P] = '"');
  if Quoted then P := P + 1;
  StartPos := P;
  if Quoted then begin
    while (P <= Length(Text)) and (Text[P] <> '"') do P := P + 1;
  end else begin
    while (P <= Length(Text)) and (Text[P] <> ',') and (Text[P] <> #13) and (Text[P] <> #10) and (Text[P] <> '}') do P := P + 1;
  end;
  EndPos := P - 1;
  if EndPos >= StartPos then Result := Trim(Copy(Text, StartPos, EndPos - StartPos + 1));
end;

procedure LoadExistingConfig;
var
  Text, Value: String;
begin
  if not LoadStringFromFile(ExpandConstant('{commonappdata}\PontoParColetor\config.json'), Text) then exit;
  Value := ReadJsonValue(Text, 'deviceIp'); if Value <> '' then DevicePage.Values[0] := Value;
  Value := ReadJsonValue(Text, 'devicePort'); if Value <> '' then DevicePage.Values[1] := Value;
  Value := ReadJsonValue(Text, 'login'); if Value <> '' then DevicePage.Values[2] := Value;
  Value := ReadJsonValue(Text, 'password'); if Value <> '' then DevicePage.Values[3] := Value;
  Value := ReadJsonValue(Text, 'deviceId'); if Value <> '' then DevicePage.Values[4] := Value;
  Value := ReadJsonValue(Text, 'theraBase'); if Value <> '' then TheraPage.Values[0] := Value;
  Value := ReadJsonValue(Text, 'deviceSecret'); if Value <> '' then TheraPage.Values[1] := Value;
  Value := ReadJsonValue(Text, 'pollSeconds'); if Value <> '' then TheraPage.Values[2] := Value;
end;

function JsonEscape(Value: String): String;
begin
  Result := Value;
  StringChangeEx(Result, '\', '\\', True);
  StringChangeEx(Result, '"', '\"', True);
  StringChangeEx(Result, #13, '', True);
  StringChangeEx(Result, #10, '\n', True);
end;

procedure InitializeWizard;
begin
  DevicePage := CreateInputQueryPage(wpSelectDir,
    'Conexão com o Control iD',
    'Informe onde está o relógio de ponto.',
    'O computador precisa estar na mesma rede do Control iD. Estes dados podem ser alterados depois pelo PontoPar.');
  DevicePage.Add('IP do Control iD:', False);
  DevicePage.Add('Porta web:', False);
  DevicePage.Add('Usuário:', False);
  DevicePage.Add('Senha:', True);
  DevicePage.Add('ID do aparelho no Thera (0 = automático):', False);
  DevicePage.Values[0] := '192.168.0.140';
  DevicePage.Values[1] := '80';
  DevicePage.Values[2] := 'admin';
  DevicePage.Values[4] := '0';

  TheraPage := CreateInputQueryPage(DevicePage.ID,
    'Conexão com o Thera',
    'Informe o endereço e o segredo do coletor.',
    'O segredo fica somente neste computador, em C:\ProgramData\PontoParColetor\config.json.');
  TheraPage.Add('Endereço do Thera:', False);
  TheraPage.Add('Segredo do Thera:', True);
  TheraPage.Add('Intervalo de coleta (segundos):', False);
  TheraPage.Values[0] := 'https://xi6vuuvift.us-east-1.awsapprunner.com';
  TheraPage.Values[2] := '15';
  LoadExistingConfig;
end;

function IsIntegerInRange(Value: String; MinValue, MaxValue: Integer): Boolean;
var Parsed: Integer;
begin
  Result := TryStrToInt(Trim(Value), Parsed) and (Parsed >= MinValue) and (Parsed <= MaxValue);
end;

function NextButtonClick(CurPageID: Integer): Boolean;
begin
  Result := True;
  if CurPageID = DevicePage.ID then begin
    if Trim(DevicePage.Values[0]) = '' then begin MsgBox('Informe o IP do Control iD.', mbError, MB_OK); Result := False; exit; end;
    if not IsIntegerInRange(DevicePage.Values[1], 1, 65535) then begin MsgBox('A porta deve ser um número entre 1 e 65535.', mbError, MB_OK); Result := False; exit; end;
    if Trim(DevicePage.Values[2]) = '' then begin MsgBox('Informe o usuário do Control iD.', mbError, MB_OK); Result := False; exit; end;
    if not IsIntegerInRange(DevicePage.Values[4], 0, 2147483647) then begin MsgBox('O ID do aparelho deve ser zero ou um número positivo.', mbError, MB_OK); Result := False; exit; end;
  end;
  if CurPageID = TheraPage.ID then begin
    if Trim(TheraPage.Values[0]) = '' then begin MsgBox('Informe o endereço do Thera.', mbError, MB_OK); Result := False; exit; end;
    if Trim(TheraPage.Values[1]) = '' then begin MsgBox('Informe o segredo do Thera.', mbError, MB_OK); Result := False; exit; end;
    if not IsIntegerInRange(TheraPage.Values[2], 1, 86400) then begin MsgBox('O intervalo deve ser um número entre 1 e 86400 segundos.', mbError, MB_OK); Result := False; exit; end;
  end;
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  ResultCode: Integer;
  CurrentExe: String;
begin
  Result := '';
  CurrentExe := ExpandConstant('{app}\pontopar-coletor.exe');
  if FileExists(CurrentExe) then
    Exec(CurrentExe, 'stop', ExpandConstant('{app}'), SW_HIDE, ewWaitUntilTerminated, ResultCode);
end;

procedure CurStepChanged(CurStep: TSetupStep);
var ConfigText: String;
begin
  if CurStep = ssInstall then begin
    ForceDirectories(ExpandConstant('{app}'));
    ConfigText := '{' + #13#10 +
      '  "deviceIp": "' + JsonEscape(Trim(DevicePage.Values[0])) + '",' + #13#10 +
      '  "devicePort": ' + Trim(DevicePage.Values[1]) + ',' + #13#10 +
      '  "login": "' + JsonEscape(Trim(DevicePage.Values[2])) + '",' + #13#10 +
      '  "password": "' + JsonEscape(DevicePage.Values[3]) + '",' + #13#10 +
      '  "deviceId": "' + JsonEscape(Trim(DevicePage.Values[4])) + '",' + #13#10 +
      '  "theraBase": "' + JsonEscape(Trim(TheraPage.Values[0])) + '",' + #13#10 +
      '  "deviceSecret": "' + JsonEscape(Trim(TheraPage.Values[1])) + '",' + #13#10 +
      '  "pollSeconds": ' + Trim(TheraPage.Values[2]) + ',' + #13#10 +
      '  "update": {' + #13#10 +
      '    "repo": "p-lacerda/pontopar-coletor",' + #13#10 +
      '    "checkHours": 6,' + #13#10 +
      '    "token": ""' + #13#10 +
      '  }' + #13#10 +
      '}' + #13#10;
    if not SaveStringToFile(ExpandConstant('{app}\config.json'), ConfigText, False) then
      RaiseException('Não foi possível salvar a configuração do PontoPar.');
  end;
end;
