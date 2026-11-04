# Installs Meiro for the current user: the installer of the latest version
# for this PC, its SHA-256 checked against SHA256SUMS of its release, run
# silently. Made by mygo build.
#
#   irm https://github.com/elianiva/meiro/releases/download/desktop-v0.5.1/install.ps1 | iex

& {
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$appName = 'Meiro'
$exeName = 'Meiro.exe'
$setupPrefix = 'meiro-setup-'
$identifier = 'com.elianiva.meiro'
$defaultFolder = Join-Path $env:LOCALAPPDATA ('Programs\' + 'Meiro')
# Where the files of the latest version are: in the release of the newest
# tag with the prefix when the repository's releases (api) hold others too.
$releases = 'https://github.com/elianiva/meiro/releases/download/'
$api = 'https://api.github.com/repos/elianiva/meiro/releases?per_page=100'
$tagPrefix = 'desktop-v'
# GitHub renames the spaces of uploaded files to dots; its API lists them.
$github = $true
$assetsApi = 'https://api.github.com/repos/elianiva/meiro/releases/tags/'
# For tests only: where the files are instead, and leaving the app closed.
$downloadUrl = $env:MEIRO_DOWNLOAD_URL
$noOpen = $env:MEIRO_NO_OPEN
if ($downloadUrl) {
	$releases = $downloadUrl.TrimEnd('/') + '/'
	$api = ''
	$github = $false
}

function Get-Arch {
	$arch = $env:PROCESSOR_ARCHITEW6432
	if (-not $arch) { $arch = $env:PROCESSOR_ARCHITECTURE }
	if ($arch -eq 'AMD64') {
		# An x64 PowerShell on an Arm PC.
		try {
			if ((Get-CimInstance Win32_Processor -ErrorAction Stop | Select-Object -First 1).Architecture -eq 12) { $arch = 'ARM64' }
		} catch {}
	}
	switch ($arch) {
		'ARM64' { return 'arm64' }
		'AMD64' { return 'amd64' }
		default { throw "$appName is not made for $arch" }
	}
}

function Get-File([string]$url, [string]$path) {
	Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile $path
}

# Get-LatestTag returns the tag of the newest release tagged $tagPrefix that
# is neither a draft nor a prerelease.
function Get-LatestTag {
	for ($page = 1; $page -le 10; $page++) {
		# Windows PowerShell 5.1 outputs the JSON array as one object, which
		# ForEach-Object enumerates.
		$list = @(Invoke-RestMethod -UseBasicParsing -Uri "$api&page=$page" | ForEach-Object { $_ })
		foreach ($r in $list) {
			if (-not $r.draft -and -not $r.prerelease -and $r.tag_name.StartsWith($tagPrefix)) { return $r.tag_name }
		}
		if ($list.Count -lt 100) { break }
	}
	throw "no release of $appName is published"
}

# Get-AppProcess returns the processes running from the folder. It finds
# them with CIM, which reads every path at once: the Path of each process
# from Get-Process takes seconds in all on Windows PowerShell 5.1.
function Get-AppProcess([string]$folder) {
	$dir = $folder.TrimEnd('\') + '\'
	Get-CimInstance Win32_Process -Property ProcessId, ExecutablePath -ErrorAction SilentlyContinue | Where-Object {
		$_.ExecutablePath -and $_.ProcessId -ne $PID -and $_.ExecutablePath.StartsWith($dir, [StringComparison]::OrdinalIgnoreCase)
	} | ForEach-Object { Get-Process -Id $_.ProcessId -ErrorAction SilentlyContinue }
}

# Wait-Exit waits up to the seconds for the processes to exit.
function Wait-Exit($processes, [int]$seconds) {
	$deadline = (Get-Date).AddSeconds($seconds)
	foreach ($p in $processes) {
		$left = [int]($deadline - (Get-Date)).TotalMilliseconds
		if ($left -lt 0) { $left = 0 }
		try { [void]$p.WaitForExit($left) } catch {}
	}
}

function Install-App {
	try {
		[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
	} catch {}
	$arch = Get-Arch
	$base = $releases
	$assets = $assetsApi
	if ($api) {
		$tag = Get-LatestTag
		$base = $releases + [uri]::EscapeDataString($tag) + '/'
		$assets = $assetsApi + [uri]::EscapeDataString($tag)
	}
	$work = Join-Path ([IO.Path]::GetTempPath()) ([IO.Path]::GetRandomFileName())
	New-Item -ItemType Directory -Path $work | Out-Null
	try {
		$sumsFile = Join-Path $work 'SHA256SUMS'
		Get-File ($base + 'SHA256SUMS') $sumsFile
		$sum = $null
		$name = $null
		$pattern = '^([0-9a-fA-F]{64}) [ *](' + [regex]::Escape($setupPrefix) + '(\S+)-' + $arch + '\.exe)$'
		foreach ($line in [IO.File]::ReadAllLines($sumsFile)) {
			if ($line.TrimEnd() -match $pattern) {
				$sum = $Matches[1]
				$name = $Matches[2]
				$version = $Matches[3]
				break
			}
		}
		if (-not $name) { throw "the release has no installer of $appName for $arch" }

		Write-Host "Downloading $appName $version for $arch"
		$setup = Join-Path $work ($name -replace ' ', '.')
		if ($github) {
			try {
				Get-File ($base + ($name -replace ' ', '.')) $setup
			} catch {
				# Not the name GitHub gave it: the release's assets name it.
				$key = $name -replace '[^0-9A-Za-z]', ''
				$asset = (Invoke-RestMethod -UseBasicParsing -Uri $assets).assets |
					Where-Object { ($_.name -replace '[^0-9A-Za-z]', '') -eq $key } | Select-Object -First 1
				if (-not $asset) { throw }
				Get-File $asset.browser_download_url $setup
			}
		} else {
			Get-File ($base + [uri]::EscapeDataString($name)) $setup
		}
		$actual = (Get-FileHash -Algorithm SHA256 -Path $setup).Hash
		if ($actual -ne $sum.ToUpperInvariant()) {
			throw "$name does not have the SHA-256 of SHA256SUMS: not installing it"
		}

		# The installed version's folder, which a silent install would
		# otherwise leave for the default one.
		$folder = $defaultFolder
		$uninstallKey = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\' + $identifier
		$location = (Get-ItemProperty -Path $uninstallKey -ErrorAction SilentlyContinue).InstallLocation
		if ($location) { $folder = $location.TrimEnd('\') }

		# Quit the app: ask its windows to close and, after 10 s, stop the
		# processes still running from its folder, as an app in the tray
		# keeps running without windows.
		$running = @(Get-AppProcess $folder)
		if ($running.Count -gt 0) {
			Write-Host "Quitting $appName"
			foreach ($p in $running) { try { [void]$p.CloseMainWindow() } catch {} }
			Wait-Exit $running 10
			$running = @(Get-AppProcess $folder)
			if ($running.Count -gt 0) {
				$running | Stop-Process -Force -ErrorAction SilentlyContinue
				Wait-Exit $running 10
				if (@(Get-AppProcess $folder).Count -gt 0) { throw "$appName is still running: quit it and run this again" }
			}
		}

		Write-Host "Installing $appName in $folder"
		# /D, last, takes the rest of the command line, unquoted.
		$process = Start-Process -FilePath $setup -ArgumentList "/S /D=$folder" -Wait -PassThru
		if ($process.ExitCode -ne 0) { throw "the installer of $appName failed with exit code $($process.ExitCode)" }
		$exe = Join-Path $folder $exeName
		if (-not (Test-Path -LiteralPath $exe)) { throw "the installer did not install $exe" }
		if (-not $noOpen) { Start-Process -FilePath $exe }
		Write-Host "Installed $appName $version in $folder"
	} finally {
		Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
	}
}

try {
	Install-App
} catch {
	Write-Host "install.ps1: $_" -ForegroundColor Red
}
}
