# ASCOM Alpaca Proxy Driver

The project also includes a standalone ASCOM Alpaca proxy driver written in Go. This application connects to the SV241 device via its serial port and exposes it to the ASCOM ecosystem as standard `Switch` and `ObservingConditions` devices.

> **Note:** While the proxy is written in Go and designed to be cross-platform (Windows, macOS, Linux), it has currently only been tested on Windows.

## Features

*   Auto-detection of the SV241 serial port.
*   Exposes all power outputs as a single ASCOM `Switch` device.
*   Exposes environmental sensors as an ASCOM `ObservingConditions` device.
*   Provides a web-based setup page for configuration.
*   Manages the connection to the device automatically.
*   Cross-platform (Windows, macOS, Linux).

## Important Security Notice

> **Warning:** All traffic between the astronomy software (client) and this Alpaca proxy driver is transmitted **unencrypted** over the network (HTTP).
>
> *   This means that **anyone on the same network** can potentially access the driver and control your device.
> *   It is strongly recommended to restrict access to the proxy port (default `8080`) using **firewall rules** on your computer. Only allow access from the computer running your control software (usually `localhost` or `127.0.0.1`).
> *   Do not use this driver on unsecured networks (e.g., public Wi-Fi).

### ASCOM Switch Device Actions

To provide functionality beyond the standard ASCOM `Switch` specification, the driver implements several custom **Actions**. These can be triggered by ASCOM client software that supports them, or manually via API calls.

#### Master Switch Actions

These actions provide a "Master Switch" to control all power outputs simultaneously.

*   `MasterSwitchOn`: Turns all power outputs on.
*   `MasterSwitchOff`: Turns all power outputs off.

#### Sensor Readout Actions

These actions allow reading the main power metrics directly from the `Switch` device, which can be convenient in some clients.

*   `getvoltage`: Returns the current input voltage (in Volts).
*   `getcurrent`: Returns the total current draw (in Amps).
*   `getpower`: Returns the total power consumption (in Watts).

#### Using Actions via API (e.g., with `curl`)

You can trigger these actions from the command line using a tool like `curl`. The endpoint for actions is `/api/v1/switch/0/action` and the method is `PUT`.

**Example: Turn all switches off**
```bash
curl -X PUT -d "Action=MasterSwitchOff" http://localhost:8080/api/v1/switch/0/action
```

**Example: Get current voltage**
```bash
curl -X PUT -d "Action=getvoltage" http://localhost:8080/api/v1/switch/0/action
```
> **Note for Windows PowerShell users:** The standard `curl` command in PowerShell is an alias for `Invoke-WebRequest`, which has a different syntax and requires the `Content-Type` to be set explicitly. To use the master switch, the correct PowerShell command is:
> ```powershell
> Invoke-WebRequest -Uri http://localhost:8080/api/v1/switch/0/action -Method PUT -Body "Action=MasterSwitchOff" -ContentType "application/x-www-form-urlencoded"
> ```

## Accessing the Setup Page

The primary way to configure the SV241 Alpaca Proxy is through its built-in web interface. There are several ways to access it:

1.  **System Tray Icon (Recommended)**
    *   When the proxy is running, a new icon will appear in your system tray (usually in the bottom-right corner of the screen on Windows).
    *   **Right-click** the icon and select **"Open Setup Page"** from the menu. This will open the correct page in your default web browser.

2.  **Direct Browser URL**
    *   You can also access the page by manually entering the URL into your web browser. By default, the address is:
    *   `http://localhost:8080/setup`

3.  **If the Default Port is Busy**
    *   The proxy is configured to start on port `8080` by default. If another application is already using this port, the proxy will automatically search for the next available port (e.g., `8081`, `8082`, etc.).
    *   If you cannot connect using the default URL, check the `proxy.log` file located in the configuration directory. The log file will contain a line indicating which port the server started on, for example:
        ```
        [INFO] Starting Alpaca API server on port 8081...
        ```
    *   You would then use that port in the URL: `http://localhost:8081/setup`


## Configuration

The proxy creates its configuration files in the standard user config directory:
*   **Windows:** `%APPDATA%\SV241AlpacaProxy\`
*   **Linux:** `~/.config/SV241AlpacaProxy/`
*   **macOS:** `~/Library/Application Support/SV241AlpacaProxy/`

### Manual Configuration (`proxy_config.json`)

While most settings can be configured via the web interface, it is also possible to edit the `proxy_config.json` file directly. This can be useful for troubleshooting or for setting up the proxy in a headless environment.

Here is an example of the `proxy_config.json` file structure:

```json
{
  "serialPortName": "COM9",
  "networkPort": 8080,
  "logLevel": "DEBUG",
  "switchNames": {
    "adj_conv": "Adjustable Voltage",
    "dc1": "Camera",
    "dc2": "Mount",
    "dc3": "Focuser",
    "dc4": "Filter Wheel",
    "dc5": "Unused",
    "pwm1": "Main Dew Heater",
    "pwm2": "Guide Scope Heater",
    "usb345": "USB Hub 1",
    "usbc12": "USB Hub 2"
  }
}
```

**Parameter Explanation:**

*   `serialPortName` (string): The name of the serial port for the SV241 device (e.g., `"COM9"` on Windows, `"/dev/ttyUSB0"` on Linux). If this string is empty (`""`), the proxy will attempt to auto-detect the port on startup.
*   `networkPort` (integer): The TCP port on which the Alpaca API server will listen for connections from client applications. The default is `8080`. A restart of the proxy is required for changes to this value to take effect.
*   `logLevel` (string): Controls the verbosity of the log file. Valid values are `"ERROR"`, `"WARN"`, `"INFO"`, and `"DEBUG"`. This setting is applied live when changed.
*   `switchNames` (object): A map that allows you to assign custom, user-friendly names to the internal switch identifiers. The `key` is the internal name (e.g., `"dc1"`) and the `value` is the custom name you want to see in ASCOM clients and the web interface.

### Log Level Configuration

The proxy driver provides a configurable logging level to control the amount of detail written to the log file (`proxy.log` in the configuration directory). This is useful for both normal operation and detailed troubleshooting. The log level can be set in the **"Proxy Connection Settings"** section of the web setup page.

The available levels are:

*   **ERROR**: Logs only critical errors that prevent the proxy from working correctly (e.g., failure to open a serial port, server start failure).
*   **WARN**: Logs warnings about non-critical issues that the proxy can recover from (e.g., a temporary connection loss, auto-detection failures).
*   **INFO** (Default): Logs major events during normal operation, such as application start/stop, successful connections, and configuration changes. This level provides a good overview without being too verbose.
*   **DEBUG**: Logs highly detailed information, including every incoming HTTP request, every command sent to the device, and periodic status checks. This level is extremely useful for diagnosing communication problems with ASCOM client software but will create very large log files.

### Log Rotation

To prevent the log file from growing indefinitely, the proxy performs a simple rotation upon every start:
1.  The existing `proxy.log` is renamed to `proxy.log.old`, overwriting any previous `.old` file.
2.  A new, empty `proxy.log` is created for the current session.

This ensures that the logs from the current and the immediately preceding session are always available for troubleshooting.
