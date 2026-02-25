param(
    [Parameter(Mandatory=$true)][string]$VirtioFlavor,
    [Parameter(Mandatory=$false)][string]$AdminUsername = "Administrator",
    [Parameter(Mandatory=$false)][string]$AdminPassword = "P@ssw0rd!",
    [Parameter(Mandatory=$false)][string]$EnableRDP = "false",
    [Parameter(Mandatory=$false)][string]$OptimizeForSize = "false",
    [Parameter(Mandatory=$false)][string]$GoldenProfile = "false"
)

$ErrorActionPreference = "Stop"
$markerDir = "C:\Windows\Temp\Packager"
$stateDir = Join-Path $markerDir "state"
$markerFile = Join-Path $markerDir "postinstall.started"

if (-not (Test-Path $markerDir)) {
    New-Item -Path $markerDir -ItemType Directory -Force | Out-Null
}
if (-not (Test-Path $stateDir)) {
    New-Item -Path $stateDir -ItemType Directory -Force | Out-Null
}

Start-Transcript -Path (Join-Path $markerDir "postinstall.log") -Append

function Mark-Stage {
    param([string]$Name)
    $stateFile = Join-Path $script:stateDir ($Name + ".ok")
    New-Item -Path $stateFile -ItemType File -Force | Out-Null
    Write-Host ("[stage] " + $Name + " OK")
}

function Has-Stage {
    param([string]$Name)
    return (Test-Path (Join-Path $script:stateDir ($Name + ".ok")))
}

function To-Bool {
    param([string]$Value)
    if ($null -eq $Value) { return $false }
    $v = $Value.Trim().ToLowerInvariant()
    return ($v -eq "1" -or $v -eq "true" -or $v -eq "$true" -or $v -eq "yes" -or $v -eq "on")
}

function Ensure-RunnerTask {
    $taskCmd = 'cmd /c if exist C:\Windows\Temp\Packager\RunPostInstall.cmd call C:\Windows\Temp\Packager\RunPostInstall.cmd'
    try {
        schtasks.exe /Create /TN "PackagerPostInstall" /SC ONSTART /RU "SYSTEM" /RL HIGHEST /TR $taskCmd /F | Out-Host
    } catch {
        Write-Host "warning: could not ensure PackagerPostInstall scheduled task: $($_.Exception.Message)"
    }
}

function Remove-RunnerTask {
    try {
        schtasks.exe /Delete /TN "PackagerPostInstall" /F | Out-Host
    } catch {}
}

function Test-RebootPending {
    $keys = @(
        "HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Component Based Servicing\RebootPending",
        "HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\WindowsUpdate\Auto Update\RebootRequired",
        "HKLM:\SYSTEM\CurrentControlSet\Control\Session Manager"
    )
    if (Test-Path $keys[0]) { return $true }
    if (Test-Path $keys[1]) { return $true }
    try {
        $pending = (Get-ItemProperty -Path $keys[2] -Name "PendingFileRenameOperations" -ErrorAction SilentlyContinue)
        if ($null -ne $pending) { return $true }
    } catch {}
    return $false
}

function Request-RebootAndExit {
    param([string]$Reason)
    Write-Host "Reboot required: $Reason"
    Set-Content -Path (Join-Path $stateDir "pending_reboot.reason") -Value $Reason
    Mark-Stage -Name "reboot_requested"
    Restart-Computer -Force
    exit 0
}

function Get-UpdateRound {
    $roundFile = Join-Path $script:stateDir "windows_update.round"
    if (-not (Test-Path $roundFile)) {
        return 0
    }
    try {
        return [int](Get-Content -Path $roundFile -Raw)
    } catch {
        return 0
    }
}

function Set-UpdateRound {
    param([int]$Value)
    if (-not (Test-Path $script:stateDir)) {
        New-Item -Path $script:stateDir -ItemType Directory -Force | Out-Null
    }
    Set-Content -Path (Join-Path $script:stateDir "windows_update.round") -Value $Value
}

function Install-WindowsUpdatesRound {
    $session = New-Object -ComObject Microsoft.Update.Session
    $searcher = $session.CreateUpdateSearcher()
    $result = $searcher.Search("IsInstalled=0 and Type='Software' and IsHidden=0")

    $candidateUpdates = New-Object -ComObject Microsoft.Update.UpdateColl
    for ($i = 0; $i -lt $result.Updates.Count; $i++) {
        $update = $result.Updates.Item($i)
        if ($update.InstallationBehavior.CanRequestUserInput) {
            continue
        }
        if (-not $update.EulaAccepted) {
            try { $update.AcceptEula() } catch {}
        }
        [void]$candidateUpdates.Add($update)
    }

    if ($candidateUpdates.Count -eq 0) {
        return @{
            Found = 0
            Installed = 0
            RebootRequired = (Test-RebootPending)
        }
    }

    $downloader = $session.CreateUpdateDownloader()
    $downloader.Updates = $candidateUpdates
    [void]$downloader.Download()

    $installer = $session.CreateUpdateInstaller()
    $installer.Updates = $candidateUpdates
    $installResult = $installer.Install()

    $installedCount = 0
    try {
        for ($j = 0; $j -lt $candidateUpdates.Count; $j++) {
            $code = $installResult.GetUpdateResult($j).ResultCode
            if ($code -eq 2 -or $code -eq 3) {
                $installedCount++
            }
        }
    } catch {
        $installedCount = $candidateUpdates.Count
    }

    return @{
        Found = $candidateUpdates.Count
        Installed = $installedCount
        RebootRequired = ($installResult.RebootRequired -or (Test-RebootPending))
    }
}

function Invoke-WindowsUpdateAutomation {
    param([int]$MaxRounds = 12)

    if (Has-Stage -Name "windows_update_done") {
        return
    }

    $round = Get-UpdateRound
    while ($round -lt $MaxRounds) {
        $round++
        Set-UpdateRound -Value $round
        Write-Host "Windows Update round $round/$MaxRounds"

        $wu = Install-WindowsUpdatesRound
        Write-Host ("Updates found: {0}, installed: {1}, reboot: {2}" -f $wu.Found, $wu.Installed, $wu.RebootRequired)

        if ($wu.RebootRequired) {
            Request-RebootAndExit -Reason ("windows_updates_round_" + $round)
        }

        if ($wu.Found -eq 0) {
            if (Test-RebootPending) {
                Request-RebootAndExit -Reason "post_update_pending_reboot"
            }
            Mark-Stage -Name "windows_update_done"
            return
        }
    }

    Write-Host "warning: reached Windows Update max rounds ($MaxRounds); continuing"
    Mark-Stage -Name "windows_update_max_rounds"
    if (Test-RebootPending) {
        Request-RebootAndExit -Reason "windows_updates_max_rounds_reboot"
    }
    Mark-Stage -Name "windows_update_done"
}

function Remove-SysprepBlockingPackages {
    $setupAct = Join-Path $env:windir "System32\Sysprep\Panther\setupact.log"
    $prefixes = New-Object 'System.Collections.Generic.HashSet[string]'

    [void]$prefixes.Add("Microsoft.MicrosoftEdge.Stable")

    if (Test-Path $setupAct) {
        try {
            $hits = Get-Content $setupAct | Select-String "failed to remove packages" -Context 0,2
            foreach ($hit in $hits) {
                $lines = @($hit.Line)
                foreach ($ctx in $hit.Context.PostContext) {
                    $lines += $ctx
                }
                foreach ($line in $lines) {
                    $matches = [regex]::Matches($line, "([A-Za-z0-9][A-Za-z0-9\._-]+_[0-9][A-Za-z0-9\._-]*_(x64|x86|neutral|arm64)_[A-Za-z0-9\._-]+)")
                    foreach ($m in $matches) {
                        $full = $m.Groups[1].Value
                        if ($full -and $full.Contains("_")) {
                            $prefix = $full.Split("_")[0]
                            if ($prefix) {
                                [void]$prefixes.Add($prefix)
                            }
                        }
                    }
                }
            }
        } catch {
            Write-Host "warning: could not parse sysprep setupact.log: $($_.Exception.Message)"
        }
    }

    foreach ($name in $prefixes) {
        Write-Host "Removing Appx package family pattern: *$name*"
        try {
            Get-AppxPackage -AllUsers -Name "*$name*" | Remove-AppxPackage -AllUsers -ErrorAction SilentlyContinue
        } catch {}
        try {
            Get-AppxProvisionedPackage -Online | Where-Object { $_.PackageName -like "*$name*" } | Remove-AppxProvisionedPackage -Online -ErrorAction SilentlyContinue | Out-Null
        } catch {}
    }

    Mark-Stage -Name "appx_cleanup_done"
}

$EnableRDPBool = To-Bool -Value $EnableRDP
$OptimizeForSizeBool = To-Bool -Value $OptimizeForSize
$GoldenProfileBool = To-Bool -Value $GoldenProfile

if (Has-Stage -Name "sysprep_launching") {
    Write-Host "Sysprep already launched. Exiting."
    exit 0
}

Ensure-RunnerTask

if (-not (Test-Path $markerFile)) {
    New-Item -Path $markerFile -ItemType File -Force | Out-Null
    Mark-Stage -Name "started"
}

function Install-InfFolder {
    param([string]$Path)
    if (Test-Path $Path) {
        Write-Host "Installing drivers from $Path"
        pnputil /add-driver "$Path\*.inf" /subdirs /install | Out-Host
    }
}

$driverRoot = "E:\"
if (-not (Test-Path (Join-Path $driverRoot "PACKAGER.TAG"))) {
    $candidates = Get-PSDrive -PSProvider FileSystem | Select-Object -ExpandProperty Root
    foreach ($candidate in $candidates) {
        if (Test-Path (Join-Path $candidate "PACKAGER.TAG")) {
            $driverRoot = $candidate
            break
        }
    }
}

Write-Host "Using driver media at $driverRoot"

if (-not (Has-Stage -Name "drivers_installed")) {
    $folders = @(
        "viostor",
        "vioscsi",
        "NetKVM",
        "Balloon",
        "vioserial",
        "qemupciserial",
        "fwcfg",
        "viorng"
    )

    foreach ($folder in $folders) {
        Install-InfFolder -Path (Join-Path $driverRoot "$folder\$VirtioFlavor\amd64")
    }
    Mark-Stage -Name "drivers_installed"
}

if (-not (Has-Stage -Name "qga_installed")) {
    $qga = Join-Path $driverRoot "qemu-ga-x86_64.msi"
    if (Test-Path $qga) {
        Write-Host "Installing QEMU Guest Agent"
        Start-Process msiexec.exe -Wait -ArgumentList @('/i', "`"$qga`"", '/qn', '/norestart')
    }
    Mark-Stage -Name "qga_installed"
}

if (-not (Has-Stage -Name "virtio_tools_installed")) {
    $virtioTools = Join-Path $driverRoot "virtio-win-gt-x64.msi"
    if (Test-Path $virtioTools) {
        Write-Host "Installing virtio-win guest tools"
        Start-Process msiexec.exe -Wait -ArgumentList @('/i', "`"$virtioTools`"", '/qn', '/norestart')
    }
    Mark-Stage -Name "virtio_tools_installed"
}

if (-not (Has-Stage -Name "cloudbase_installed")) {
    $cloudbase = Join-Path $driverRoot "CloudbaseInitSetup.msi"
    if (Test-Path $cloudbase) {
        Write-Host "Installing Cloudbase-Init"
        Start-Process msiexec.exe -Wait -ArgumentList @('/i', "`"$cloudbase`"", '/qn', '/norestart')
    }
    Mark-Stage -Name "cloudbase_installed"
}

if ($GoldenProfileBool -and -not (Has-Stage -Name "golden_profile_applied")) {
    Write-Host "Applying golden profile defaults"
    try { tzutil /s "UTC" | Out-Host } catch {}
    try { powercfg /h off | Out-Host } catch {}

    $cloudbaseConfRoot = "C:\Program Files\Cloudbase Solutions\Cloudbase-Init\conf"
    $confFiles = @(
        Join-Path $cloudbaseConfRoot "cloudbase-init.conf",
        Join-Path $cloudbaseConfRoot "cloudbase-init-unattend.conf"
    )
    foreach ($conf in $confFiles) {
        if (-not (Test-Path $conf)) {
            continue
        }

        try {
            $raw = Get-Content $conf -Raw
        } catch {
            continue
        }

        if ($raw -match "(?m)^metadata_services=") {
            $raw = [regex]::Replace($raw, "(?m)^metadata_services=.*$", "metadata_services=cloudbaseinit.metadata.services.nocloudservice.NoCloudConfigDriveService")
        } else {
            $raw += "`r`nmetadata_services=cloudbaseinit.metadata.services.nocloudservice.NoCloudConfigDriveService"
        }

        if ($raw -match "(?m)^plugins=") {
            $raw = [regex]::Replace($raw, "(?m)^plugins=.*$", "plugins=cloudbaseinit.plugins.common.userdata.UserDataPlugin")
        } else {
            $raw += "`r`nplugins=cloudbaseinit.plugins.common.userdata.UserDataPlugin"
        }

        if ($raw -match "(?m)^stop_service_on_exit=") {
            $raw = [regex]::Replace($raw, "(?m)^stop_service_on_exit=.*$", "stop_service_on_exit=true")
        } else {
            $raw += "`r`nstop_service_on_exit=true"
        }

        if ($raw -match "(?m)^fail_on_userdata_error=") {
            $raw = [regex]::Replace($raw, "(?m)^fail_on_userdata_error=.*$", "fail_on_userdata_error=false")
        } else {
            $raw += "`r`nfail_on_userdata_error=false"
        }

        Set-Content -Path $conf -Value $raw
    }

    Mark-Stage -Name "golden_profile_applied"
}

if (-not (Has-Stage -Name "timezone_set")) {
    try { tzutil /s "UTC" | Out-Host } catch {}
    Mark-Stage -Name "timezone_set"
}

if (-not (Has-Stage -Name "trim_enabled")) {
    $trimOutput = ""
    try {
        $trimOutput = (fsutil behavior query DisableDeleteNotify | Out-String)
    } catch {}

    if ($trimOutput -notmatch "=\s*0") {
        try { fsutil behavior set DisableDeleteNotify 0 | Out-Host } catch {}
    }
    Mark-Stage -Name "trim_enabled"
}

if (-not (Has-Stage -Name "server_manager_disabled")) {
    try { reg add "HKLM\SOFTWARE\Microsoft\ServerManager" /v DoNotOpenServerManagerAtLogon /t REG_DWORD /d 1 /f | Out-Host } catch {}
    try { reg add "HKCU\SOFTWARE\Microsoft\ServerManager" /v DoNotOpenServerManagerAtLogon /t REG_DWORD /d 1 /f | Out-Host } catch {}
    Mark-Stage -Name "server_manager_disabled"
}

if ($AdminUsername -and $AdminUsername -ne "Administrator" -and -not (Has-Stage -Name "custom_admin_ready")) {
    $existing = Get-LocalUser -Name $AdminUsername -ErrorAction SilentlyContinue
    if (-not $existing) {
        Write-Host "Creating local admin user $AdminUsername"
        net user $AdminUsername "$AdminPassword" /add | Out-Host
    }
    net localgroup Administrators $AdminUsername /add | Out-Host
    Mark-Stage -Name "custom_admin_ready"
}

if ($EnableRDPBool -and -not (Has-Stage -Name "rdp_enabled")) {
    Write-Host "Enabling RDP"
    reg add "HKLM\System\CurrentControlSet\Control\Terminal Server" /v fDenyTSConnections /t REG_DWORD /d 0 /f | Out-Host
    netsh advfirewall firewall set rule group="remote desktop" new enable=Yes | Out-Host
    Mark-Stage -Name "rdp_enabled"
}

if (-not (Has-Stage -Name "dism_cleanup_done")) {
    Write-Host "Running component cleanup"
    Start-Process dism.exe -Wait -ArgumentList @('/Online', '/Cleanup-Image', '/StartComponentCleanup', '/ResetBase')
    Mark-Stage -Name "dism_cleanup_done"
}

if (-not (Has-Stage -Name "windows_update_done")) {
    try {
        Invoke-WindowsUpdateAutomation
    } catch {
        Write-Host "warning: Windows Update automation failed: $($_.Exception.Message)"
        Mark-Stage -Name "windows_update_failed"
        if (Test-RebootPending) {
            Request-RebootAndExit -Reason "windows_update_failed_reboot_pending"
        }
        Mark-Stage -Name "windows_update_done"
    }
}

if (-not (Has-Stage -Name "appx_cleanup_done")) {
    Remove-SysprepBlockingPackages
}

if ($OptimizeForSizeBool -and -not (Has-Stage -Name "zero_fill_done")) {
    Write-Host "OptimizeForSize enabled: zero-filling free space on C:"

    $sdelete = Join-Path $driverRoot "sdelete64.exe"
    if (-not (Test-Path $sdelete)) {
        $sdelete = Join-Path $driverRoot "sdelete.exe"
    }

    if (Test-Path $sdelete) {
        Start-Process -FilePath $sdelete -Wait -ArgumentList @('-accepteula', '-nobanner', '-z', 'C:')
    } else {
        Write-Host "sdelete not found on driver media, using cipher fallback"
        Start-Process cipher.exe -Wait -ArgumentList @('/w:C')
    }
    Mark-Stage -Name "zero_fill_done"
}

if (Test-RebootPending) {
    Request-RebootAndExit -Reason "pre_sysprep_reboot_pending"
}

if (-not (Test-Path "C:\Windows\Panther\Sysprep-Unattend.xml")) {
    Copy-Item (Join-Path $PSScriptRoot "Sysprep-Unattend.xml") "C:\Windows\Panther\Sysprep-Unattend.xml" -Force
}

Write-Host "Launching sysprep"
Mark-Stage -Name "sysprep_launching"
Remove-RunnerTask
Start-Process "C:\Windows\System32\Sysprep\Sysprep.exe" -Wait -ArgumentList @('/generalize', '/oobe', '/shutdown', '/mode:vm', '/unattend:C:\Windows\Panther\Sysprep-Unattend.xml')
