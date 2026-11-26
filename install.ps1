#Requires -Version 5.1
#Requires -RunAsAdministrator

[CmdletBinding(DefaultParameterSetName = 'Install')]
param(
    [Parameter(Mandatory, ParameterSetName = 'Install')]
    [ValidatePattern('\A[0-9]+\.[0-9]+\.[0-9]+\z')]
    [string]$Version,

    [Parameter(ParameterSetName = 'Install')]
    [string]$ReleaseDir = (Get-Location).Path,

    [Parameter(Mandatory, ParameterSetName = 'Remove')]
    [switch]$Remove
)

$ErrorActionPreference = 'Stop'
$taskName = 'pullgate'
$root = Join-Path ([Environment]::GetFolderPath('CommonApplicationData')) 'pullgate'
$installed = Join-Path $root 'pullgate.exe'
$repo = 'code6x/pullgate'

function Register-PullgateTask {
    param([string]$Executable, [string]$Directory, [string]$Name)
    $action = New-ScheduledTaskAction -Execute $Executable -Argument 'agent' -WorkingDirectory $Directory
    $trigger = New-ScheduledTaskTrigger -AtStartup
    $principal = New-ScheduledTaskPrincipal -UserId 'S-1-5-18' -LogonType ServiceAccount -RunLevel Highest
    $settings = New-ScheduledTaskSettingsSet -MultipleInstances IgnoreNew -ExecutionTimeLimit ([TimeSpan]::Zero) `
        -RestartCount 10 -RestartInterval (New-TimeSpan -Minutes 1) -StartWhenAvailable `
        -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
    Register-ScheduledTask -TaskName $Name -TaskPath '\' -Action $action -Trigger $trigger `
        -Principal $principal -Settings $settings -Force | Out-Null
}

function Stop-PullgateTask {
    $task = Get-ScheduledTask -TaskName $taskName -TaskPath '\' -ErrorAction SilentlyContinue
    if ($task -and $task.State -eq 'Running') {
        Stop-ScheduledTask -InputObject $task
        for ($i = 0; $i -lt 30; $i++) {
            $current = Get-ScheduledTask -TaskName $taskName -TaskPath '\'
            if ($current.State -ne 'Running') { return }
            Start-Sleep -Milliseconds 200
        }
        throw 'The pullgate task did not stop. Stop it in Task Scheduler and retry.'
    }
}

if ($Remove) {
    Stop-PullgateTask
    $task = Get-ScheduledTask -TaskName $taskName -TaskPath '\' -ErrorAction SilentlyContinue
    if ($task) {
        Unregister-ScheduledTask -InputObject $task -Confirm:$false
    }
    # Leave the protected directory for recovery and audit. The agent and
    # its automatic startup are gone as soon as the task is unregistered.
    Write-Output "Removed the pullgate task. The files are still in $root."
    return
}

$archiveName = "pullgate_${Version}_windows_amd64.tar.gz"
# Use the elevated operator's private profile for the staging directory.
$stage = Join-Path ([Environment]::GetFolderPath('LocalApplicationData')) ("pullgate-" + [Guid]::NewGuid().ToString('N'))
New-Item -Path $stage -ItemType Directory | Out-Null
try {
    foreach ($name in @($archiveName, 'SHA256SUMS', "pullgate_${Version}.sigstore.json")) {
        Copy-Item -LiteralPath (Join-Path $ReleaseDir $name) -Destination $stage
    }
    $archive = Join-Path $stage $archiveName
    $checksums = Join-Path $stage 'SHA256SUMS'
    $bundle = Join-Path $stage "pullgate_${Version}.sigstore.json"
    $pattern = '\A([0-9a-fA-F]{64})  ' + [regex]::Escape($archiveName) + '\z'
    $hashes = @(Get-Content -LiteralPath $checksums | ForEach-Object {
        if ($_ -cmatch $pattern) { $Matches[1] }
    })
    if ($hashes.Count -ne 1 -or (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash -ne $hashes[0]) {
        throw 'The Windows archive failed its SHA-256 check. Download all three files from the same release.'
    }

    $gh = Join-Path ([Environment]::GetFolderPath('ProgramFiles')) 'GitHub CLI\gh.exe'
    if (-not (Test-Path -LiteralPath $gh -PathType Leaf)) {
        throw 'GitHub CLI is missing. Install it for all users in Program Files.'
    }
    & $gh attestation verify $archive --bundle $bundle --repo $repo `
        --cert-identity "https://github.com/$repo/.github/workflows/release.yml@refs/heads/main" `
        --deny-self-hosted-runners
    if ($LASTEXITCODE -ne 0) {
        throw 'The release attestation failed. Download the archive and bundle from the same release.'
    }

    $tar = Join-Path ([Environment]::SystemDirectory) 'tar.exe'
    & $tar -xf $archive -C $stage 'pullgate.exe'
    if ($LASTEXITCODE -ne 0) { throw 'Could not extract the verified Windows archive. Download it again.' }
    $binary = Join-Path $stage 'pullgate.exe'
    $reported = & $binary version
    if ($LASTEXITCODE -ne 0 -or $reported -cne "pullgate $Version") {
        throw 'The binary version does not match the requested release.'
    }
    $old = Get-ScheduledTask -TaskName $taskName -TaskPath '\' -ErrorAction SilentlyContinue
    Stop-PullgateTask
    & $binary install-binary
    if ($LASTEXITCODE -ne 0) { throw 'The protected binary did not install. Read the pullgate error above and retry.' }

    Register-PullgateTask -Executable $installed -Directory $root -Name $taskName

    if (-not $old -or $old.State -eq 'Disabled') {
        Disable-ScheduledTask -TaskName $taskName -TaskPath '\' | Out-Null
        Write-Output "Installed pullgate. Run setup and doctor, then enable and start the task."
    } elseif ($old.State -eq 'Running') {
        Start-ScheduledTask -TaskName $taskName -TaskPath '\'
    }
} finally {
    Remove-Item -LiteralPath $stage -Recurse -Force
}
