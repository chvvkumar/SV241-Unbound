<#
.SYNOPSIS
    Interactively creates a dynamic ASCOM Alpaca driver for the SV241 Proxy.
.DESCRIPTION
    This script guides the user through creating an ASCOM driver for either the Switch
    or ObservingConditions device exposed by the SV241 Alpaca Proxy.
    It automatically finds the required ASCOM DLL, reads the configured network port
    from the proxy's config file, and calls the official ASCOM Platform utility
    to perform a full COM registration of the driver.
    This script MUST be run as an Administrator.
.NOTES
    Author: Gemini
    Last Modified: 2025-11-12
    Recommended to be launched via the corresponding 'Create-Driver.bat' file.
#>

# --- 0. Set Window Title and Clear Screen ---
$Host.UI.RawUI.WindowTitle = "ASCOM Driver Creator for SV241"
Clear-Host

# --- 1. Verify Administrator Privileges ---
# This script is designed to be launched via a .bat file that already requests elevation.
# This check is a fallback.
$currentUser = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-Not $currentUser.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Host "------------------------------------------------------------------" -ForegroundColor Red
    Write-Host "ERROR: Administrator privileges are required." -ForegroundColor Red
    Write-Host "Please right-click 'Create-Driver.bat' and select 'Run as administrator'." -ForegroundColor Yellow
    Write-Host "------------------------------------------------------------------" -ForegroundColor Red
    Read-Host "Press Enter to exit"
    exit
}

# --- 2. Locate and Load ASCOM Assembly ---
Write-Host "Locating ASCOM Platform utilities..." -ForegroundColor Cyan
$AscomUtilPath = "C:\Program Files (x86)\Common Files\ASCOM\AlpacaDynamicClients\ASCOM.Com.dll"

if (-Not (Test-Path $AscomUtilPath)) {
    Write-Host "------------------------------------------------------------------" -ForegroundColor Red
    Write-Host "ERROR: Could not find the ASCOM utility DLL." -ForegroundColor Red
    Write-Host "Please ensure the ASCOM Platform is installed correctly." -ForegroundColor Yellow
    Write-Host "Expected path: $AscomUtilPath" -ForegroundColor Yellow
    Write-Host "------------------------------------------------------------------" -ForegroundColor Red
    Read-Host "Press Enter to exit"
    exit
}

try {
    Add-Type -Path $AscomUtilPath
    Write-Host "ASCOM utilities loaded successfully." -ForegroundColor Green
} catch {
    Write-Host "------------------------------------------------------------------" -ForegroundColor Red
    Write-Host "FATAL: Failed to load the ASCOM utility DLL." -ForegroundColor Red
    Write-Host "Error details: $_" -ForegroundColor Yellow
    Write-Host "------------------------------------------------------------------" -ForegroundColor Red
    Read-Host "Press Enter to exit"
    exit
}

# --- 3. Determine IP Address and Port ---
$IP = "127.0.0.1" # Default IP
$Port = 8080       # Default Port
$ProxyConfigPath = Join-Path $env:APPDATA "SV241AlpacaProxy\proxy_config.json"
$useManualEntry = $false

if (Test-Path $ProxyConfigPath) {
    try {
        $ProxyConfig = Get-Content -Raw -Path $ProxyConfigPath | ConvertFrom-Json
        $foundIP = "127.0.0.1" # Default if listenAddress is 0.0.0.0
        if ($ProxyConfig.listenAddress -and $ProxyConfig.listenAddress -ne "0.0.0.0") {
            $foundIP = $ProxyConfig.listenAddress
        }
        $foundPort = $ProxyConfig.networkPort | ForEach-Object { $_ -f "####" } # Ensure it's a number

        Write-Host "------------------------------------------------------------------" -ForegroundColor White
        Write-Host "Proxy configuration found on this PC." -ForegroundColor Cyan
        Write-Host "  - Detected IP Address: $foundIP"
        Write-Host "  - Detected Port:       $foundPort"
        Write-Host "------------------------------------------------------------------" -ForegroundColor White

        $choice = Read-Host "Do you want to use these settings? (Y/n)"
        if ($choice -eq 'n' -or $choice -eq 'N') {
            $useManualEntry = $true
        } else {
            $IP = $foundIP
            $Port = $foundPort
        }
    } catch {
        Write-Host "Warning: Could not read or parse '$ProxyConfigPath'. Manual entry required." -ForegroundColor Yellow
        $useManualEntry = $true
    }
} else {
    Write-Host "Proxy configuration not found on this PC. Please enter details manually." -ForegroundColor Cyan
    $useManualEntry = $true
}

if ($useManualEntry) {
    Write-Host "------------------------------------------------------------------" -ForegroundColor White
    # --- Manual IP Input ---
    while ($true) {
        $manualIP = Read-Host "Enter the IP Address of the computer running the SV241 Proxy [$IP]"
        if ([string]::IsNullOrWhiteSpace($manualIP)) {
            $manualIP = $IP
        }
        try {
            [System.Net.IPAddress]::Parse($manualIP) | Out-Null
            $IP = $manualIP
            break
        } catch {
            Write-Host "ERROR: '$manualIP' is not a valid IP address. Please try again." -ForegroundColor Red
        }
    }

    # --- Manual Port Input ---
    while ($true) {
        $manualPort = Read-Host "Enter the Port Number of the SV241 Proxy [$Port]"
        if ([string]::IsNullOrWhiteSpace($manualPort)) {
            $manualPort = $Port
        }
        if ($manualPort -match "^\d{1,5}$" -and [int]$manualPort -ge 1 -and [int]$manualPort -le 65535) {
            $Port = [int]$manualPort
            break
        } else {
            Write-Host "ERROR: '$manualPort' is not a valid port number (1-65535). Please try again." -ForegroundColor Red
        }
    }
}


# --- 4. User Input ---
Write-Host "------------------------------------------------------------------" -ForegroundColor White

# --- Driver Type ---
$deviceTypeChoice = Read-Host "Select driver type to create (1=Switch, 2=ObservingConditions) [1]"
if ($deviceTypeChoice -eq '2') {
    $DeviceType = [ASCOM.Common.DeviceTypes]::ObservingConditions
    $DefaultName = "SV241 Environment"
} else {
    $DeviceType = [ASCOM.Common.DeviceTypes]::Switch
    $DefaultName = "SV241 Power Switch"
}

# --- Driver Name ---
$DisplayName = Read-Host "Enter a name for the driver in ASCOM [$DefaultName]"
if ([string]::IsNullOrWhiteSpace($DisplayName)) {
    $DisplayName = $DefaultName
}

# --- 5. Define Remaining Parameters and Create Driver ---
$DeviceNum = 0
$UniqueID = [guid]::NewGuid().ToString()

Write-Host "------------------------------------------------------------------" -ForegroundColor White
Write-Host "Creating driver with the following settings:"
Write-Host "  - Name: $DisplayName"
Write-Host "  - Type: $DeviceType"
Write-Host "  - Target: $IP`:$Port"
Write-Host "  - Device Number: $DeviceNum"
Write-Host "..."

try {
    # This is the official 6-argument method from ASCOM.Com.dll
    $ProgID = [ASCOM.Com.PlatformUtilities]::CreateDynamicDriver($DeviceType, $DeviceNum, $DisplayName, $IP, $Port, $UniqueID)
    
    Write-Host "------------------------------------------------------------------" -ForegroundColor Green
    Write-Host "SUCCESS! The ASCOM driver was created." -ForegroundColor Green
    Write-Host "You can now select '$DisplayName' in your astronomy software."
    Write-Host "Registered ProgID: $ProgID"
    Write-Host "------------------------------------------------------------------" -ForegroundColor Green

} catch {
    Write-Host "------------------------------------------------------------------" -ForegroundColor Red
    Write-Host "ERROR: Failed to create the ASCOM driver." -ForegroundColor Red
    Write-Host "Error details: $_" -ForegroundColor Yellow
    Write-Host "Please ensure the ASCOM Platform is installed and not corrupted." -ForegroundColor Yellow
    Write-Host "------------------------------------------------------------------" -ForegroundColor Red
}

Read-Host "Press Enter to exit"