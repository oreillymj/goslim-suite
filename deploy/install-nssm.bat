@echo off
rem Install GoSlimPlayer as a Windows service with NSSM (https://nssm.cc).
rem Run from an administrator prompt after copying goslimplayer.exe and config.json to INSTALLDIR.
setlocal
set "INSTALLDIR=C:\GoSlimPlayer"
set "SVC=GoSlimPlayer"

nssm install %SVC% "%INSTALLDIR%\goslimplayer.exe" -u "%INSTALLDIR%\config.json"
nssm set %SVC% AppDirectory "%INSTALLDIR%"
nssm set %SVC% DisplayName "GoSlimPlayer"
nssm set %SVC% Description "Squeezebox player presenting UPnP renderers to Lyrion Music Server"
nssm set %SVC% Start SERVICE_AUTO_START

rem Log output (stdout and stderr) to a rotating file.
nssm set %SVC% AppStdout "%INSTALLDIR%\goslimplayer.log"
nssm set %SVC% AppStderr "%INSTALLDIR%\goslimplayer.log"
nssm set %SVC% AppRotateFiles 1
nssm set %SVC% AppRotateOnline 1
nssm set %SVC% AppRotateBytes 5242880

rem Stopping: NSSM sends Ctrl+C first. The program leaves LMS and stops the speakers,
rem which takes a few seconds, so allow it 10 s before NSSM would terminate it.
nssm set %SVC% AppStopMethodConsole 10000
nssm set %SVC% AppStopMethodWindow 0
nssm set %SVC% AppStopMethodThreads 0

rem Restart it if it exits, after 5 s.
nssm set %SVC% AppExit Default Restart
nssm set %SVC% AppRestartDelay 5000

nssm start %SVC%
