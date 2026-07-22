Unicode true

!define REQUEST_EXECUTION_LEVEL "user"
!define WAILS_INSTALL_SCOPE "user"
!include "x64.nsh"
!include "WinVer.nsh"
!include "LogicLib.nsh"
!include "FileFunc.nsh"
!include "Sections.nsh"

!ifndef INFO_PROJECTNAME
    !define INFO_PROJECTNAME "DouyinLiveDesktop"
!endif
!ifndef INFO_COMPANYNAME
    !define INFO_COMPANYNAME "DouyinLive"
!endif
!ifndef INFO_PRODUCTNAME
    !define INFO_PRODUCTNAME "DouyinLive Desktop"
!endif
!ifndef INFO_PRODUCTVERSION
    !define INFO_PRODUCTVERSION "0.1.0"
!endif
!ifndef INFO_COPYRIGHT
    !define INFO_COPYRIGHT "Copyright (c) 2026"
!endif
!ifndef PRODUCT_EXECUTABLE
    !define PRODUCT_EXECUTABLE "douyin-live-desktop.exe"
!endif
!ifndef UNINST_KEY_NAME
    !define UNINST_KEY_NAME "${INFO_COMPANYNAME}${INFO_PROJECTNAME}"
!endif
!define UNINST_KEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\${UNINST_KEY_NAME}"
!define ARCH "amd64"

RequestExecutionLevel user

!macro wails.checkArchitecture
    ${IfNot} ${AtLeastWin10}
        IfSilent unsupportedWindowsSilent unsupportedWindowsUI
        unsupportedWindowsSilent:
            SetErrorLevel 64
            Quit
        unsupportedWindowsUI:
            MessageBox MB_OK "This product requires Windows 10 or later."
            Quit
    ${EndIf}
    ${IfNot} ${IsNativeAMD64}
        IfSilent unsupportedArchitectureSilent unsupportedArchitectureUI
        unsupportedArchitectureSilent:
            SetErrorLevel 65
            Quit
        unsupportedArchitectureUI:
            MessageBox MB_OK "This package requires Windows x64."
            Quit
    ${EndIf}
!macroend

!macro wails.setShellContext
    SetShellVarContext current
!macroend

!macro wails.files
    File "/oname=${PRODUCT_EXECUTABLE}" "${ARG_WAILS_AMD64_BINARY}"
!macroend

!macro wails.writeUninstaller
    WriteUninstaller "$INSTDIR\uninstall.exe"
    SetRegView 64
    WriteRegStr HKCU "${UNINST_KEY}" "Publisher" "${INFO_COMPANYNAME}"
    WriteRegStr HKCU "${UNINST_KEY}" "DisplayName" "${INFO_PRODUCTNAME}"
    WriteRegStr HKCU "${UNINST_KEY}" "DisplayVersion" "${INFO_PRODUCTVERSION}"
    WriteRegStr HKCU "${UNINST_KEY}" "DisplayIcon" "$INSTDIR\${PRODUCT_EXECUTABLE}"
    WriteRegStr HKCU "${UNINST_KEY}" "InstallLocation" "$INSTDIR"
    WriteRegStr HKCU "${UNINST_KEY}" "UninstallString" '"$INSTDIR\uninstall.exe"'
    WriteRegStr HKCU "${UNINST_KEY}" "QuietUninstallString" '"$INSTDIR\uninstall.exe" /S'
    WriteRegDWORD HKCU "${UNINST_KEY}" "NoModify" 1
    WriteRegDWORD HKCU "${UNINST_KEY}" "NoRepair" 1
    ${GetSize} "$INSTDIR" "/S=0K" $0 $1 $2
    IntFmt $0 "0x%08X" $0
    WriteRegDWORD HKCU "${UNINST_KEY}" "EstimatedSize" "$0"
!macroend

!macro wails.deleteUninstaller
    Delete "$INSTDIR\uninstall.exe"
    SetRegView 64
    DeleteRegKey HKCU "${UNINST_KEY}"
    SetRegView 32
    DeleteRegKey HKCU "${UNINST_KEY}"
!macroend

!ifndef ARG_WAILS_AMD64_BINARY
    !error "ARG_WAILS_AMD64_BINARY is required"
!endif
!ifndef ARG_FFMPEG_BINARY
    !error "ARG_FFMPEG_BINARY is required"
!endif
!ifndef ARG_FFPROBE_BINARY
    !error "ARG_FFPROBE_BINARY is required"
!endif
!ifndef ARG_DBROLLBACK_BINARY
    !error "ARG_DBROLLBACK_BINARY is required"
!endif
!ifndef ARG_LICENSE_FILE
    !error "ARG_LICENSE_FILE is required"
!endif
!ifndef ARG_LICENSE_MANIFEST
    !error "ARG_LICENSE_MANIFEST is required"
!endif
!ifndef ARG_NOTICES_FILE
    !error "ARG_NOTICES_FILE is required"
!endif
!ifndef ARG_SBOM_FILE
    !error "ARG_SBOM_FILE is required"
!endif
!ifndef ARG_FFMPEG_LOCK
    !error "ARG_FFMPEG_LOCK is required"
!endif
!ifndef ARG_INSTALLATION_GUIDE
    !error "ARG_INSTALLATION_GUIDE is required"
!endif
!ifndef ARG_INSTALLER_OUTPUT
    !define ARG_INSTALLER_OUTPUT "..\..\bin\${INFO_PROJECTNAME}-${ARCH}-installer.exe"
!endif
!ifndef DOUYINLIVE_DATA_ROOT
    !define DOUYINLIVE_DATA_ROOT "$LOCALAPPDATA\DouyinLive"
!endif

!define DOUYINLIVE_WEBVIEW2_URL "https://go.microsoft.com/fwlink/p/?LinkId=2124703"
!define DOUYINLIVE_WEBVIEW2_MISSING_EXIT 74
!define DOUYINLIVE_PURGE_CONFIRM_EXIT 75

VIProductVersion "${INFO_PRODUCTVERSION}.0"
VIFileVersion    "${INFO_PRODUCTVERSION}.0"
VIAddVersionKey "CompanyName"     "${INFO_COMPANYNAME}"
VIAddVersionKey "FileDescription" "${INFO_PRODUCTNAME} Installer"
VIAddVersionKey "ProductVersion"  "${INFO_PRODUCTVERSION}"
VIAddVersionKey "FileVersion"     "${INFO_PRODUCTVERSION}"
VIAddVersionKey "LegalCopyright"  "${INFO_COPYRIGHT}"
VIAddVersionKey "ProductName"     "${INFO_PRODUCTNAME}"

ManifestDPIAware true

!include "MUI.nsh"
!define MUI_ICON "..\icon.ico"
!define MUI_UNICON "..\icon.ico"
!define MUI_FINISHPAGE_NOAUTOCLOSE
!define MUI_ABORTWARNING

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_LICENSE "${ARG_LICENSE_FILE}"
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_COMPONENTS
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "SimpChinese"
!insertmacro MUI_LANGUAGE "English"

Name "${INFO_PRODUCTNAME}"
OutFile "${ARG_INSTALLER_OUTPUT}"
InstallDir "$LOCALAPPDATA\Programs\${INFO_PRODUCTNAME}"
ShowInstDetails show
ShowUninstDetails show

Var PurgeMode

Function .onInit
    !insertmacro wails.checkArchitecture
    Call CheckWebView2
FunctionEnd

Section "Install ${INFO_PRODUCTNAME}" SecInstall
    !insertmacro wails.setShellContext
    SetOverwrite on
    SetOutPath $INSTDIR
    !insertmacro wails.files
    File "/oname=douyin-live-dbrollback.exe" "${ARG_DBROLLBACK_BINARY}"

    SetOutPath "$INSTDIR\ffmpeg"
    File "/oname=ffmpeg.exe" "${ARG_FFMPEG_BINARY}"
    File "/oname=ffprobe.exe" "${ARG_FFPROBE_BINARY}"

    SetOutPath "$INSTDIR\licenses"
    File "/oname=LICENSE.txt" "${ARG_LICENSE_FILE}"
    File "/oname=licenses.json" "${ARG_LICENSE_MANIFEST}"
    File "/oname=THIRD-PARTY-NOTICES.txt" "${ARG_NOTICES_FILE}"
    File "/oname=sbom.spdx.json" "${ARG_SBOM_FILE}"
    File "/oname=ffmpeg-windows-amd64.lock.json" "${ARG_FFMPEG_LOCK}"
    File "/oname=INSTALLATION.md" "${ARG_INSTALLATION_GUIDE}"

    SetOutPath $INSTDIR
    CreateShortcut "$SMPROGRAMS\${INFO_PRODUCTNAME}.lnk" "$INSTDIR\${PRODUCT_EXECUTABLE}"
    CreateShortCut "$DESKTOP\${INFO_PRODUCTNAME}.lnk" "$INSTDIR\${PRODUCT_EXECUTABLE}"
    !insertmacro wails.writeUninstaller
SectionEnd

Section "un.Uninstall ${INFO_PRODUCTNAME}" SecUninstall
    !insertmacro wails.setShellContext
    SetRegView 64
    DeleteRegKey HKCU "${UNINST_KEY}"
    SetRegView 32
    DeleteRegKey HKCU "${UNINST_KEY}"
    RMDir /r "$AppData\${PRODUCT_EXECUTABLE}"
    RMDir /r $INSTDIR
    Delete "$SMPROGRAMS\${INFO_PRODUCTNAME}.lnk"
    Delete "$DESKTOP\${INFO_PRODUCTNAME}.lnk"
    !insertmacro wails.deleteUninstaller
    StrCmp $PurgeMode "confirmed" 0 uninstallDone
    RMDir /r "${DOUYINLIVE_DATA_ROOT}"
    IfFileExists "${DOUYINLIVE_DATA_ROOT}\*.*" uninstallPurgeFailed uninstallDone
    uninstallPurgeFailed:
        SetErrorLevel ${DOUYINLIVE_PURGE_CONFIRM_EXIT}
        Quit
    uninstallDone:
SectionEnd

Section /o "un.Delete database, internal media, configuration and logs" SecPurgeData
    IfSilent silentPurge interactivePurge
    interactivePurge:
        ${GetSize} "${DOUYINLIVE_DATA_ROOT}" "/S=0K" $0 $1 $2
        MessageBox MB_ICONEXCLAMATION|MB_YESNO|MB_DEFBUTTON2 \
            "This permanently deletes local data (estimated $0 KiB), including the database and internal media. External media is not deleted. Continue?" \
            IDYES purgeData IDNO skipPurge
    silentPurge:
        StrCmp $PurgeMode "confirmed" purgeData purgeDenied
    purgeDenied:
        SetErrorLevel ${DOUYINLIVE_PURGE_CONFIRM_EXIT}
        Quit
    purgeData:
        RMDir /r "${DOUYINLIVE_DATA_ROOT}"
        IfFileExists "${DOUYINLIVE_DATA_ROOT}\*.*" purgeFailed purgeDone
    purgeFailed:
        SetErrorLevel ${DOUYINLIVE_PURGE_CONFIRM_EXIT}
        Quit
    purgeDone:
    skipPurge:
SectionEnd

Function CheckWebView2
    StrCpy $0 ""
    !ifndef DOUYINLIVE_FORCE_WEBVIEW2_MISSING
        SetRegView 64
        ReadRegStr $0 HKLM "SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}" "pv"
        ${If} $0 == ""
            ReadRegStr $0 HKLM "SOFTWARE\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}" "pv"
        ${EndIf}
        ${If} $0 == ""
            ReadRegStr $0 HKCU "Software\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}" "pv"
        ${EndIf}
        SetRegView 32
        ${If} $0 == ""
            ReadRegStr $0 HKLM "SOFTWARE\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}" "pv"
        ${EndIf}
    !endif
    ${If} $0 == ""
        IfSilent webviewMissingSilent webviewMissingInteractive
        webviewMissingInteractive:
            MessageBox MB_ICONEXCLAMATION|MB_OKCANCEL \
                "Microsoft Edge WebView2 Evergreen Runtime is required. Select OK to open the official installer, install it, then run this setup again." \
                IDOK openWebView2 IDCANCEL webviewMissingAbort
        openWebView2:
            ExecShell "open" "${DOUYINLIVE_WEBVIEW2_URL}"
        webviewMissingAbort:
            SetErrorLevel ${DOUYINLIVE_WEBVIEW2_MISSING_EXIT}
            Quit
        webviewMissingSilent:
            SetErrorLevel ${DOUYINLIVE_WEBVIEW2_MISSING_EXIT}
            Quit
    ${EndIf}
FunctionEnd

Function un.onInit
    StrCpy $PurgeMode "preserve"
    !ifdef DOUYINLIVE_MANAGED_PURGE_TEST
        ReadEnvStr $0 "DOUYINLIVE_PURGE_DATA"
        StrCmp $0 "1" 0 purgeOptionsDone
        StrCpy $PurgeMode "requested"
        ReadEnvStr $0 "DOUYINLIVE_CONFIRM_PURGE"
        StrCmp $0 "1" 0 purgeConfirmationDenied
        StrCpy $PurgeMode "confirmed"
        Goto purgeOptionsDone
        purgeConfirmationDenied:
            SetErrorLevel ${DOUYINLIVE_PURGE_CONFIRM_EXIT}
            Quit
        purgeOptionsDone:
    !endif
FunctionEnd
