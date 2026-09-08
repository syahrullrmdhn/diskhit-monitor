@echo off
setlocal EnableExtensions
title DiskHist Monitor Updater

net session >nul 2>&1
if errorlevel 1 (
  echo Meminta hak Administrator...
  start "" /wait powershell -NoProfile -ExecutionPolicy Bypass -Command "Start-Process -FilePath '%~f0' -Verb RunAs -Wait"
  exit /b
)

set "SERVICE=DiskHistMonitor"
set "BASE_URL=https://github.com/syahrullrmdhn/diskhit-monitor/releases/latest/download"
set "WORK=%TEMP%\diskhist-update-%RANDOM%"
set "NEW_EXE=%WORK%\diskhist-windows-amd64.exe"
set "SUMS=%WORK%\SHA256SUMS.txt"
set "PATH_FILE=%WORK%\install-path.txt"
set "BACKUP_ROOT=%ProgramData%\DiskHistMonitor\backup"

mkdir "%WORK%" >nul 2>&1
mkdir "%BACKUP_ROOT%" >nul 2>&1
if errorlevel 1 goto :fail_prepare

echo.
echo [1/6] Mencari instalasi DiskHist Monitor...
powershell -NoProfile -ExecutionPolicy Bypass -Command "$ErrorActionPreference='Stop'; $raw=(Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Services\%SERVICE%').ImagePath; $p=[Environment]::ExpandEnvironmentVariables($raw).Trim().Trim([char]34); [IO.File]::WriteAllText('%PATH_FILE%',$p)" >nul 2>&1
if errorlevel 1 goto :fail_not_installed
set /p "TARGET_EXE="<"%PATH_FILE%"
if not exist "%TARGET_EXE%" goto :fail_not_installed
for %%I in ("%TARGET_EXE%") do set "INSTALL_DIR=%%~dpI"

echo [2/6] Mengunduh versi terbaru...
powershell -NoProfile -ExecutionPolicy Bypass -Command "$ErrorActionPreference='Stop'; [Net.ServicePointManager]::SecurityProtocol=[Net.SecurityProtocolType]::Tls12; Invoke-WebRequest '%BASE_URL%/diskhist-windows-amd64.exe' -OutFile '%NEW_EXE%'; Invoke-WebRequest '%BASE_URL%/SHA256SUMS.txt' -OutFile '%SUMS%'" 
if errorlevel 1 goto :fail_download

echo [3/6] Memverifikasi checksum...
powershell -NoProfile -ExecutionPolicy Bypass -Command "$ErrorActionPreference='Stop'; $line=Get-Content '%SUMS%' | Where-Object {$_ -match 'diskhist-windows-amd64\.exe$'} | Select-Object -First 1; if(!$line){throw 'Checksum EXE tidak ditemukan'}; $expected=($line -split '\s+')[0].ToUpperInvariant(); $actual=(Get-FileHash '%NEW_EXE%' -Algorithm SHA256).Hash; if($actual -ne $expected){throw 'Checksum tidak cocok'}" 
if errorlevel 1 goto :fail_checksum

echo [4/6] Membuat backup konfigurasi, database, dan EXE lama...
copy /y "%TARGET_EXE%" "%BACKUP_ROOT%\diskhist.exe.previous" >nul
if errorlevel 1 goto :fail_backup
if exist "%INSTALL_DIR%config.yaml" copy /y "%INSTALL_DIR%config.yaml" "%BACKUP_ROOT%\config.yaml.backup" >nul
if exist "%ProgramData%\DiskHistMonitor\diskhist.db" copy /y "%ProgramData%\DiskHistMonitor\diskhist.db" "%BACKUP_ROOT%\diskhist.db.backup" >nul

echo [5/6] Menghentikan service dan memasang versi terbaru...
sc.exe stop "%SERVICE%" >nul 2>&1
powershell -NoProfile -ExecutionPolicy Bypass -Command "$limit=(Get-Date).AddMinutes(2); do {$s=(Get-Service '%SERVICE%').Status; if($s -eq 'Stopped'){exit 0}; Start-Sleep 2} while((Get-Date) -lt $limit); exit 1" 
if errorlevel 1 goto :rollback
copy /y "%NEW_EXE%" "%TARGET_EXE%" >nul
if errorlevel 1 goto :rollback
sc.exe start "%SERVICE%" >nul 2>&1
if errorlevel 1 goto :rollback

echo [6/6] Memeriksa kesehatan aplikasi...
powershell -NoProfile -ExecutionPolicy Bypass -Command "$limit=(Get-Date).AddMinutes(2); do {try {$r=Invoke-WebRequest 'http://localhost:8787/healthz' -UseBasicParsing -TimeoutSec 5; if($r.StatusCode -eq 200){exit 0}} catch {}; Start-Sleep 3} while((Get-Date) -lt $limit); exit 1" 
if errorlevel 1 goto :rollback

echo.
echo ==============================================
echo UPDATE BERHASIL
echo Dashboard: http://localhost:8787
echo Data dan config lama tetap dipertahankan.
echo Backup: %BACKUP_ROOT%
echo ==============================================
goto :cleanup_success

:rollback
echo.
echo Update gagal. Menjalankan ROLLBACK ke EXE sebelumnya...
sc.exe stop "%SERVICE%" >nul 2>&1
powershell -NoProfile -ExecutionPolicy Bypass -Command "Start-Sleep 3" >nul 2>&1
copy /y "%BACKUP_ROOT%\diskhist.exe.previous" "%TARGET_EXE%" >nul 2>&1
sc.exe start "%SERVICE%" >nul 2>&1
echo Versi sebelumnya sudah dipulihkan. Cek log di:
echo %ProgramData%\DiskHistMonitor\agent.log
goto :fail

:fail_prepare
echo Gagal membuat folder kerja atau backup.
goto :fail
:fail_not_installed
echo Service %SERVICE% tidak ditemukan. Script ini hanya untuk update instalasi lama.
goto :fail
:fail_download
echo Gagal mengunduh release terbaru. Periksa internet lalu coba lagi.
goto :fail
:fail_checksum
echo Checksum tidak valid. Update dibatalkan tanpa mengubah instalasi.
goto :fail
:fail_backup
echo Gagal membuat backup. Update dibatalkan tanpa mengubah instalasi.
goto :fail

:cleanup_success
rmdir /s /q "%WORK%" >nul 2>&1
echo.
pause
exit /b 0

:fail
rmdir /s /q "%WORK%" >nul 2>&1
echo.
pause
exit /b 1
