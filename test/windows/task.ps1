# Run on an isolated Windows CI runner. Exercise the installer's exact task
# registration function independently of the release signing check.
$ErrorActionPreference = 'Stop'
$tokens = $null
$errors = $null
$installer = [System.Management.Automation.Language.Parser]::ParseFile(
    (Join-Path $PSScriptRoot '../../install.ps1'), [ref]$tokens, [ref]$errors)
if ($errors.Count) { throw ($errors | Out-String) }
$definition = $installer.Find({
    param($node)
    $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and
        $node.Name -eq 'Register-PullgateTask'
}, $false)
if (-not $definition) { throw 'Missing task registration function.' }

$name = 'pullgate-test-' + [Guid]::NewGuid().ToString('N')
$binary = Join-Path ([Environment]::SystemDirectory) 'cmd.exe'
try {
    $register = $definition.Body.GetScriptBlock()
    & $register -Executable $binary -Directory ([Environment]::SystemDirectory) -Name $name
    $task = Get-ScheduledTask -TaskName $name -TaskPath '\'
    if ($task.Actions.Execute -ne $binary -or $task.Actions.Arguments -ne 'agent') {
        throw 'Task has the wrong action.'
    }
    if ($task.Triggers.CimClass.CimClassName -ne 'MSFT_TaskBootTrigger') {
        throw 'Task does not start at boot.'
    }
    if ($task.Principal.LogonType -ne 'ServiceAccount') {
        throw 'Task does not run as a service account.'
    }
    $account = $task.Principal.UserId
    $sid = if ($account -match '^S-1-') { $account } else {
        [System.Security.Principal.NTAccount]::new($account).Translate([System.Security.Principal.SecurityIdentifier]).Value
    }
    if ($sid -ne 'S-1-5-18') { throw 'Task does not run as SYSTEM.' }
    if ($task.Settings.ExecutionTimeLimit -ne 'PT0S') { throw 'Task has a run time limit.' }
} finally {
    Unregister-ScheduledTask -TaskName $name -TaskPath '\' -Confirm:$false -ErrorAction SilentlyContinue
}
