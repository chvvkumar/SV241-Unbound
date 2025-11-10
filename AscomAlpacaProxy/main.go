package main

import (
	"bufio"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall" // Re-add syscall for direct MessageBoxW
	"time"
	"unsafe" // Needed for syscall.UTF16PtrFromString

	"golang.org/x/sys/windows" // Keep this for LockFileEx constants

	"fyne.io/systray"
	"github.com/gorilla/websocket"
	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

// LogLevel type
type LogLevel int

const (
	LogLevelError LogLevel = iota
	LogLevelWarn
	LogLevelInfo
	LogLevelDebug
)

// --- WebSocket Hub for Live Logging ---
var hub *Hub // Global instance of the hub

// Hub maintains the set of active clients and broadcasts messages to the
// clients.
type Hub struct {
	// Registered clients.
	clients map[*Client]bool

	// Inbound messages from the clients.
	broadcast chan []byte

	// Register requests from the clients.
	register chan *Client

	// Unregister requests from clients.
	unregister chan *Client
}

// Client is a middleman between the websocket connection and the hub.
type Client struct {
	hub *Hub

	// The websocket connection.
	conn *websocket.Conn

	// Buffered channel of outbound messages.
	send chan []byte
}

func newHub() *Hub {
	return &Hub{
		broadcast:  make(chan []byte),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		clients:    make(map[*Client]bool),
	}
}

func (h *Hub) run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
			}
		case message := <-h.broadcast:
			for client := range h.clients {
				select {
				case client.send <- message:
				default:
					close(client.send)
					delete(h.clients, client)
				}
			}
		}
	}
}

// --- Log Broadcaster ---
// logBroadcaster implements io.Writer and sends log messages to the WebSocket hub.
type logBroadcaster struct{}

func (lb *logBroadcaster) Write(p []byte) (n int, err error) {
	if hub != nil {
		// To avoid blocking the logger, we send this in a non-blocking way.
		// If the hub's broadcast channel is full, the message is dropped.
		// This is a safe trade-off for a logging feature.
		select {
		case hub.broadcast <- p:
		default:
			// Hub is busy, drop log message to prevent blocking.
		}
	}
	return len(p), nil
}

// --- Command Queue ---
type SerialCommand struct {
	Command  string
	Response chan<- string
	Error    chan<- error
}

var (
	highPriorityCommands = make(chan SerialCommand)
	lowPriorityCommands  = make(chan SerialCommand)
)

// --- Global State ---
var (
	firmwareVersion     string
	sv241Port           serial.Port
	logFile             *os.File
	portMutex           = &sync.Mutex{}
	clientTransactionID uint32
	serverTransactionID uint32
	proxyConfig         = &ProxyConfig{} // Global instance of proxy configuration
	currentLogLevel     = LogLevelInfo   // Default log level
	statusCache         = &StatusCache{RWMutex: &sync.RWMutex{}}
	conditionsCache     = &ConditionsCache{RWMutex: &sync.RWMutex{}}
	switchIDMap         = map[int]string{
		0: "dc1", 1: "dc2", 2: "dc3", 3: "dc4", 4: "dc5",
		5: "usbc12", 6: "usb345", 7: "adj_conv", 8: "pwm1", 9: "pwm2",
	} // Behält die langen Namen für die interne Logik und die ASCOM-Schnittstelle
	shortSwitchIDMap = map[string]string{
		"dc1": "d1", "dc2": "d2", "dc3": "d3", "dc4": "d4", "dc5": "d5",
		"usbc12": "u12", "usb345": "u34", "adj_conv": "adj", "pwm1": "pwm1", "pwm2": "pwm2",
	}
	shortSwitchKeyByID = map[int]string{
		0: "d1", 1: "d2", 2: "d3", 3: "d4", 4: "d5",
		5: "u12", 6: "u34", 7: "adj", 8: "pwm1", 9: "pwm2",
	}
	singleInstanceMutex windows.Handle // Global variable for the named mutex handle
)

// getNetworkPortFromConfig reads the proxy_config.json file to find the network port.
// This is a minimal version of loadProxyConfig, designed to be called before logging is set up.
// It returns the port number or a default value (8080) if any error occurs.
func getNetworkPortFromConfig() int {
	const defaultPort = 8080

	configDir, err := os.UserConfigDir()
	if err != nil {
		return defaultPort
	}

	configFile := filepath.Join(configDir, "SV241AlpacaProxy", "proxy_config.json")

	file, err := os.ReadFile(configFile)
	if err != nil {
		return defaultPort
	}

	var config struct {
		NetworkPort int `json:"networkPort"`
	}
	if err := json.Unmarshal(file, &config); err != nil {
		return defaultPort
	}

	if config.NetworkPort == 0 {
		return defaultPort
	}

	return config.NetworkPort
}

// checkSingleInstance attempts to create a named mutex to ensure only one instance of the application is running.
// If another instance is running, or if the mutex cannot be created for any reason, it opens the setup page and exits.
func checkSingleInstance() {
	mutexName := "SV241AlpacaProxySingleInstanceMutex" // A unique name for the mutex
	handle, err := windows.CreateMutex(nil, true, windows.StringToUTF16Ptr(mutexName))

	// The only case where we want to continue running is if we successfully created a *new* mutex.
	// This happens when the call succeeds (err == nil) AND it didn't already exist.
	lastError := windows.GetLastError()

	if err == nil && lastError != windows.ERROR_ALREADY_EXISTS {
		// This is the first instance. We have acquired the mutex.
		// Assign the handle to the global var so it can be released on exit.
		singleInstanceMutex = handle
		return
	}

	// In all other cases (mutex already exists, or any other error creating it),
	// we open the setup page and exit, as requested by the user.
	port := getNetworkPortFromConfig()
	url := fmt.Sprintf("http://localhost:%d/setup", port)
	exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()

	// If we got a handle (even to an existing mutex), we should close it.
	if handle != 0 {
		windows.CloseHandle(handle)
	}

	os.Exit(0)
}

// showMessageBox is a helper function to display a Windows message box.

func showMessageBox(title, message string, style uint) {

	user32 := syscall.NewLazyDLL("user32.dll")

	messageBoxW := user32.NewProc("MessageBoxW")

	// Convert Go strings to UTF16 pointers for WinAPI

	lpText := syscall.StringToUTF16Ptr(message)

	lpCaption := syscall.StringToUTF16Ptr(title)

	messageBoxW.Call(0, uintptr(unsafe.Pointer(lpText)), uintptr(unsafe.Pointer(lpCaption)), uintptr(style))

}

// --- Memory Logging State ---

var (
	lastLoggedHeapFree float64

	lastLoggedHeapMinFree float64

	lastLoggedHeapMaxAlloc float64

	lastLoggedHeapSize float64

	lastMemoryLogTime time.Time
)

// ProxyConfig stores configuration specific to the Go proxy itself.

type ProxyConfig struct {
	SerialPortName string `json:"serialPortName"`

	NetworkPort int `json:"networkPort"`

	LogLevel string `json:"logLevel"`

	SwitchNames map[string]string `json:"switchNames"`

	HeaterAutoEnableLeader map[string]bool `json:"heaterAutoEnableLeader"`
}

//go:embed setup.html

var setupHTML embed.FS

// --- Data Structures ---

type StatusCache struct {
	Data map[string]interface{}

	*sync.RWMutex
}

type ConditionsCache struct {
	Data map[string]interface{}

	*sync.RWMutex
}

// AlpacaDescription defines the structure for the management/v1/description endpoint.

type AlpacaDescription struct {
	ServerName string `json:"ServerName"`

	Manufacturer string `json:"Manufacturer"`

	ManufacturerVersion string `json:"ManufacturerVersion"`

	Location string `json:"Location"`
}

// AlpacaConfiguredDevice defines the structure for a single device in the management/v1/configureddevices endpoint.

type AlpacaConfiguredDevice struct {
	DeviceName string `json:"DeviceName"`

	DeviceType string `json:"DeviceType"`

	DeviceNumber int `json:"DeviceNumber"`

	UniqueID string `json:"UniqueID"`
}

// CombinedConfig defines the structure for a full backup file.

type CombinedConfig struct {
	ProxyConfig *ProxyConfig `json:"proxyConfig"`

	FirmwareConfig json.RawMessage `json:"firmwareConfig"`
}

// --- Main Application ---

func main() {

	checkSingleInstance() // Call this at the very beginning

	systray.Run(onReady, onExit)

}

func onReady() {

	systray.SetIcon(iconData)

	systray.SetTitle("SV241 Alpaca Proxy")
	systray.SetTooltip("SV241 Alpaca Proxy Driver is running")

	mSetup := systray.AddMenuItem("Open Setup Page", "Open the web setup page")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Exit", "Quit the application")

	// Start the core application logic in a goroutine
	go startApp()

	// Handle menu clicks in a loop
	for {
		select {
		case <-mSetup.ClickedCh:
			openBrowser(fmt.Sprintf("http://localhost:%d/setup", proxyConfig.NetworkPort))
		case <-mQuit.ClickedCh:
			systray.Quit()
			return
		}
	}
}

func onExit() {
	// Cleanup tasks can go here
	logInfo("Exiting application.")
	if logFile != nil {
		log.Println("Closing log file.")
		logFile.Close()
	}

	// Release single instance mutex
	if singleInstanceMutex != 0 {
		logInfo("Releasing single instance mutex.")
		windows.ReleaseMutex(singleInstanceMutex)
		windows.CloseHandle(singleInstanceMutex)
	}
}

// serveWs handles websocket requests from the peer.
func serveWs(w http.ResponseWriter, r *http.Request) {
	var upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			// Allow all connections by default. For production, you might want to
			// check the origin of the request.
			return true
		},
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		logError("Failed to upgrade to websocket: %v", err)
		return
	}
	client := &Client{hub: hub, conn: conn, send: make(chan []byte, 256)}
	client.hub.register <- client

	// Allow collection of memory referenced by the caller by doing all work in
	// new goroutines.
	go client.writePump()
	go client.readPump()
}

// readPump pumps messages from the websocket connection to the hub.
//
// The application runs readPump in a per-connection goroutine. The application
// ensures that there is at most one reader on a connection by executing all
// reads from this goroutine.
func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c
		c.conn.Close()
	}()
	c.conn.SetReadLimit(512)
	c.conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.conn.SetPongHandler(func(string) error { c.conn.SetReadDeadline(time.Now().Add(60 * time.Second)); return nil })
	for {
		_, _, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				logError("websocket read error: %v", err)
			}
			break
		}
	}
}

// writePump pumps messages from the hub to the websocket connection.
//
// A goroutine running writePump is started for each connection. The
// application ensures that there is at most one writer to a connection by
// executing all writes from this goroutine.
func (c *Client) writePump() {
	ticker := time.NewTicker(50 * time.Second)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				// The hub closed the channel.
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)

			// Add queued chat messages to the current websocket message.
			n := len(c.send)
			for i := 0; i < n; i++ {
				w.Write(<-c.send)
			}

			if err := w.Close(); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

var proxyConfigFile string // Global variable for the config file path

func setupFileLogger() error {
	configDir, err := os.UserConfigDir()
	if err != nil {
		// This is a critical failure. We can't log to a file, so we try to show a message.
		// This will only be visible if not running in -H=windowsgui mode.
		log.Printf("[ERROR] FATAL: Could not get user config directory: %v", err)
		return fmt.Errorf("could not get user config directory: %w", err)
	}

	appConfigDir := filepath.Join(configDir, "SV241AlpacaProxy")
	if err := os.MkdirAll(appConfigDir, 0755); err != nil {
		log.Printf("FATAL: Could not create application config directory '%s': %v", appConfigDir, err)
		return fmt.Errorf("could not create application config directory: %w", err)
	}

	// Set the global paths for log and config files
	logFilePath := filepath.Join(appConfigDir, "proxy.log")
	oldLogFilePath := filepath.Join(appConfigDir, "proxy.log.old")
	proxyConfigFile = filepath.Join(appConfigDir, "proxy_config.json")

	// --- Log Rotation ---
	// 1. Check if an old log file exists and remove it.
	if _, err := os.Stat(oldLogFilePath); err == nil {
		os.Remove(oldLogFilePath)
	}
	// 2. Check if the current log file exists.
	if _, err := os.Stat(logFilePath); err == nil {
		// 3. Rename current log to old log.
		if err := os.Rename(logFilePath, oldLogFilePath); err != nil {
			log.Printf("[WARN] Failed to rotate log file: %v", err) // Use direct log before helpers are confirmed ready
		}
	}

	var errOpen error
	logFile, errOpen = os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if errOpen != nil {
		return fmt.Errorf("could not open log file: %w", errOpen)
	}

	// Create a MultiWriter to output to both the file and our WebSocket broadcaster.
	log.SetOutput(io.MultiWriter(logFile, &logBroadcaster{}))
	// Set standard log flags to include date and time.
	log.SetFlags(log.LstdFlags)

	log.Println("---")
	log.Printf("[INFO] --- Log session started at %s ---", time.Now().Format(time.RFC3339))

	return nil
}

// startApp contains the original main logic.
func startApp() {
	// Start the WebSocket hub in a goroutine.
	hub = newHub()
	go hub.run()

	if err := setupFileLogger(); err != nil {
		// If file logger setup fails, we can't do much more.
		return
	}

	// The command processor must be started before any part of the app
	// that might send a command.
	go processSerialCommands()

	// Load configuration as early as possible to apply settings like LogLevel.
	if err := loadProxyConfig(); err != nil {
		logAndExit("FATAL: Failed to load proxy configuration: %v", err)
	}

	logInfo("===========================================================")
	logInfo("==          SV241 Pro ASCOM Alpaca Proxy Driver          ==")
	logInfo("===========================================================")
	logInfo("Starting SV241 Pro ASCOM Alpaca Proxy Driver...")

	// Create a channel to signal when initialization is complete
	initDone := make(chan struct{})

	// Start background tasks that wait for the init signal
	go respondToAlpacaDiscovery() // This is independent
	go manageConnection(initDone)
	go periodicCacheUpdater(initDone)

	// Perform an initial, synchronous connection attempt to solve race condition on startup.
	logInfo("Performing initial device connection attempt...")
	if proxyConfig.SerialPortName != "" {
		logInfo("Initial Connection: Trying configured port '%s'.", proxyConfig.SerialPortName)
		portMutex.Lock()
		reconnectSerialPort(proxyConfig.SerialPortName)
		portMutex.Unlock()
	} else {
		logInfo("Initial Connection: Starting auto-detection...")
		foundPort, err := findSV241Port()
		if err != nil {
			logWarn("Initial Connection: Auto-detection failed: %v", err)
		} else {
			logInfo("Auto-detection found device on port %s. Connecting...", foundPort)
			portMutex.Lock()
			reconnectSerialPort(foundPort)
			portMutex.Unlock()
		}
	}

	portMutex.Lock()
	if sv241Port != nil {
		logInfo("Initial connection attempt finished successfully.")
	} else {
		logWarn("WARN: Initial connection attempt failed. The application will continue to try connecting in the background.")
	}
	portMutex.Unlock()

	// Now that the first connection attempt is done, signal other goroutines to start their work
	logInfo("Signaling background tasks to start main loops.")
	close(initDone)

	// Setup and start the web server
	setupHttpHandlers()

	// Get the firmware version
	go getFirmwareVersion()

	startServer() // This is a blocking call
}

func getFirmwareVersion() {
	// Wait a few seconds for the serial port to be ready
	time.Sleep(3 * time.Second)
	logInfo("Requesting firmware version from device...")
	resp, err := sendCommandToDevice(`{"get":"version"}`, false, 0)
	if err != nil {
		logWarn("Could not get firmware version: %v", err)
		firmwareVersion = "unknown"
		return
	}

	var versionResponse struct {
		Version string `json:"version"`
	}

	if err := json.Unmarshal([]byte(resp), &versionResponse); err != nil {
		logWarn("Could not parse firmware version response: %v", err)
		firmwareVersion = "unknown"
		return
	}

	firmwareVersion = versionResponse.Version
	logInfo("Firmware version: %s", firmwareVersion)
}

// startServer finds an open port and starts the HTTP server.
// It will try to use the configured port, but will search for the next available one if it's in use.
func startServer() {
	const maxPortRetries = 100
	initialPort := proxyConfig.NetworkPort

	for i := 0; i < maxPortRetries; i++ {
		addr := fmt.Sprintf(":%d", proxyConfig.NetworkPort)
		listener, err := net.Listen("tcp", addr)

		if err == nil {
			// Port is available
			logInfo("Starting Alpaca API server on port %d...", proxyConfig.NetworkPort)

			// If we found a different port than originally configured, save it.
			if proxyConfig.NetworkPort != initialPort {
				logInfo("Port %d was in use. Using new port %d and saving it to configuration.", initialPort, proxyConfig.NetworkPort)
				if err := saveProxyConfig(); err != nil {
					logWarn("Failed to save new network port to config file: %v", err)
				}
			}

			// http.Serve is a blocking call. It will run until the server is shut down.
			if err := http.Serve(listener, nil); err != nil {
				logAndExit("FATAL: HTTP server failed: %v", err)
			}
			return // Exit startServer function if http.Serve ever returns.
		}

		// If we are here, net.Listen failed. Assume the port is in use and try the next one.
		// This is a pragmatic approach since specific error checking (syscall.EADDRINUSE) has proven unreliable
		// on some platforms/localizations. The loop is bounded by maxPortRetries, so it's safe.
		logWarn("Could not bind to port %d (reason: %v). Trying next port...", proxyConfig.NetworkPort, err)
		proxyConfig.NetworkPort++ // Try the next port
		// The loop will continue to the next iteration.
	}

	// If the loop completes, we've failed to find a port after maxPortRetries.
	logAndExit("FATAL: Could not find an open port after %d retries. Please check your system configuration.", maxPortRetries)
}

func setupHttpHandlers() {
	// Redirect root path to setup page
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/setup", http.StatusFound)
		} else {
			http.NotFound(w, r)
		}
	})
	// Management API
	http.HandleFunc("/management/v1/description", handleManagementDescription)
	http.HandleFunc("/management/v1/configureddevices", handleManagementConfiguredDevices)
	http.HandleFunc("/management/apiversions", handleManagementApiVersions)

	// Setup Page & API
	http.HandleFunc("/setup", handleSetupPage)
	// Redirects for ASCOM client setup requests to the main setup page
	http.HandleFunc("/setup/v1/switch/0/setup", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/setup", http.StatusFound)
	})
	http.HandleFunc("/setup/v1/observingconditions/0/setup", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/setup", http.StatusFound)
	})
	http.HandleFunc("/api/v1/config", handleGetConfig)
	http.HandleFunc("/api/v1/config/set", handleSetConfig)
	http.HandleFunc("/api/v1/power/status", handleGetPowerStatus)
	http.HandleFunc("/api/v1/status", handleGetLiveStatus)
	http.HandleFunc("/api/v1/power/all", handleSetAllPower)
	http.HandleFunc("/api/v1/command", handleDeviceCommand)
	http.HandleFunc("/api/v1/firmware/version", handleGetFirmwareVersion)

	// Proxy Configuration API
	http.HandleFunc("/api/v1/proxy/config", handleGetProxyConfig)
	http.HandleFunc("/api/v1/proxy/config/set", handleSetProxyConfig)

	// Backup and Restore API
	http.HandleFunc("/api/v1/backup/create", handleCreateBackup)
	http.HandleFunc("/api/v1/backup/restore", handleRestoreBackup)

	// WebSocket endpoint for live logging
	http.HandleFunc("/ws/logs", serveWs)

	// --- Device API Mux ---
	// This new structure ensures that any undefined endpoint for a device returns a
	// proper HTTP 404 Not Found, which is crucial for passing conformance tests.

	// Common handlers used by multiple device types
	commonHandlers := map[string]http.HandlerFunc{
		"description":      handleDeviceDescription,
		"driverinfo":       handleDriverInfo,
		"driverversion":    handleDriverVersion,
		"connected":        handleConnected,
		"interfaceversion": handleInterfaceVersion,
		// "supportedactions" is now handled per-device to allow for device-specific actions.
	}

	// --- Switch Device ---
	switchHandlers := map[string]http.HandlerFunc{
		"maxswitch":            handleSwitchMaxSwitch,
		"maxswitchvalue":       handleSwitchMaxSwitchValue,
		"minswitchvalue":       handleSwitchMinSwitchValue,
		"getswitch":            handleSwitchGetSwitch,
		"switchstep":           handleSwitchSwitchStep,
		"getswitchvalue":       handleSwitchGetSwitchValue,
		"setswitchvalue":       handleSwitchSetSwitchValue,
		"setswitch":            handleSwitchSetSwitchValue, // Route setswitch to the same handler
		"getswitchname":        handleSwitchGetSwitchName,
		"setswitchname":        handleSwitchSetSwitchName,
		"getswitchdescription": handleSwitchGetSwitchDescription, // Use a dedicated handler for the description
		"canwrite":             handleSwitchCanWrite,
		"name":                 handleDeviceName("SV241 Power Switch"),
		"supportedactions":     handleSwitchSupportedActions,
		"action":               handleSwitchAction,
	}
	// Merge common handlers into switch handlers
	for k, v := range commonHandlers {
		switchHandlers[k] = v
	}
	http.HandleFunc("/api/v1/switch/0/", alpacaHandler(deviceMux(switchHandlers)))

	// --- ObservingConditions Device ---
	obsCondHandlers := map[string]http.HandlerFunc{
		// Implemented properties
		"temperature":      handleObsCondTemperature,
		"humidity":         handleObsCondHumidity,
		"dewpoint":         handleObsCondDewPoint,
		"name":             handleDeviceName("SV241 Environment"),
		"supportedactions": handleSupportedActions, // Generic handler (returns empty list)

		// Properties that are not implemented but require parameter validation for conformance
		"averageperiod":       handleObsCondAveragePeriod,
		"sensordescription":   handleObsCondSensorDescription,
		"timesincelastupdate": handleObsCondTimeSinceLastUpdate,
		"refresh":             handleObsCondRefresh,

		// Other known properties that are not implemented
		"cloudcover":     handleObsCondNotImplemented,
		"pressure":       handleObsCondNotImplemented,
		"rainrate":       handleObsCondNotImplemented,
		"skybrightness":  handleObsCondNotImplemented,
		"skyquality":     handleObsCondNotImplemented,
		"skytemperature": handleObsCondNotImplemented,
		"starfwhm":       handleObsCondNotImplemented,
		"winddirection":  handleObsCondNotImplemented,
		"windgust":       handleObsCondNotImplemented,
		"windspeed":      handleObsCondNotImplemented,
	}
	// Merge common handlers into observing conditions handlers
	for k, v := range commonHandlers {
		obsCondHandlers[k] = v
	}
	http.HandleFunc("/api/v1/observingconditions/0/", alpacaHandler(deviceMux(obsCondHandlers)))
}

// deviceMux creates a handler that routes to sub-handlers based on the final URL path segment.
// This is the key to correctly handling undefined methods with an HTTP 404.
func deviceMux(handlers map[string]http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Extract the method name from the end of the URL path
		path := strings.TrimSuffix(r.URL.Path, "/")
		lastSlash := strings.LastIndex(path, "/")
		if lastSlash == -1 {
			alpacaErrorResponse(w, r, http.StatusNotFound, "Invalid URL path.")
			return
		}
		method := strings.ToLower(path[lastSlash+1:])

		if handler, ok := handlers[method]; ok {
			handler(w, r) // Call the specific handler for the method
		} else {
			// If the method is not in our map of known handlers, it's a 404 Not Found.
			alpacaErrorResponse(w, r, http.StatusNotFound, fmt.Sprintf("Method '%s' not found on this device.", method))
		}
	}
}

// --- Background Tasks ---

// processSerialCommands is the heart of the command prioritization system.
// It runs in a dedicated goroutine and is the *only* part of the application
// that directly communicates with the serial port.
func processSerialCommands() {
	logInfo("Serial command processor started.")
	for {
		var cmd SerialCommand
		// The outer select non-blockingly checks for high-priority commands.
		select {
		case cmd = <-highPriorityCommands:
			// High-priority command received, process it immediately.
		default:
			// No high-priority commands waiting. Now, block and wait for either a
			// high-priority or a low-priority command.
			select {
			case cmd = <-highPriorityCommands:
				// A high-priority command arrived while we were waiting.
			case cmd = <-lowPriorityCommands:
				// A low-priority command is ready to be processed.
			}
		}

		// At this point, `cmd` holds a command to be executed.
		portMutex.Lock()
		if sv241Port == nil {
			portMutex.Unlock()
			cmd.Error <- errors.New("serial port is not open")
			continue // Go back to the top of the loop
		}

		logDebug("Processing command: %s", cmd.Command)
		_, err := sv241Port.Write([]byte(cmd.Command + "\n"))
		if err != nil {
			logError("Serial write failed: %v. Marking port as disconnected.", err)
			handleDisconnect() // Assumes handleDisconnect is safe to call here
			portMutex.Unlock()
			cmd.Error <- fmt.Errorf("failed to write to serial port: %w", err)
			continue
		}

		sv241Port.SetReadTimeout(2 * time.Second)
		reader := bufio.NewReader(sv241Port)
		response, err := reader.ReadString('\n')
		if err != nil {
			logError("Serial read failed: %v. Marking port as disconnected.", err)
			handleDisconnect()
			portMutex.Unlock()
			cmd.Error <- fmt.Errorf("failed to read from serial port: %w", err)
			continue
		}

		portMutex.Unlock()

		trimmedResponse := strings.TrimSpace(response)
		logDebug("Received response from device: %s", trimmedResponse)
		cmd.Response <- trimmedResponse
	}
}

// performCacheUpdate fetches the latest status and conditions from the device and updates the caches.
// This function is designed to be called as a one-off operation.
func performCacheUpdate() {
	logDebug("Performing on-demand cache update.") // Low priority
	statusJSON, err := sendCommandToDevice(`{"get":"status"}`, false, 0)
	if err == nil {
		var statusData map[string]map[string]interface{}
		if json.Unmarshal([]byte(statusJSON), &statusData) == nil {
			logDebug("Successfully unmarshaled status cache data.")
			statusCache.Lock()
			statusCache.Data = statusData["status"] // This is correct, the device nests it.
			statusCache.Unlock()
		} else {
			logWarn("Failed to unmarshal status JSON from device. Raw data: %s", statusJSON)
		}
	} else {
		logWarn("Failed to get status for cache update: %v", err)
	}

	conditionsJSON, err := sendCommandToDevice(`{"get":"sensors"}`, false, 0) // Low priority
	if err == nil {
		var conditionsData map[string]interface{} // This is correct, the 'j' command is not nested.
		if err := json.Unmarshal([]byte(conditionsJSON), &conditionsData); err == nil {
			conditionsCache.Lock() // The device nests the response under a "sensors" key
			conditionsCache.Data = conditionsData
			logDebug("Successfully unmarshaled conditions cache data.")
			logMemoryStatus(conditionsData) // Log memory status if needed
			conditionsCache.Unlock()
		} else {
			logWarn("Failed to unmarshal conditions JSON from device. Raw data: %s", conditionsJSON)
		}
	} else {
		logWarn("Failed to get conditions for cache update: %v", err)
	}
}

// periodicCacheUpdater is a long-running task that periodically triggers a cache update.
func periodicCacheUpdater(initDone chan struct{}) {
	logInfo("Periodic cache update task started. Waiting for initial connection...")
	<-initDone // Wait for the initial connection to be established
	logInfo("Initial connection complete. Starting cache updates.")

	for {
		performCacheUpdate()
		time.Sleep(3 * time.Second)
	}
}

// logMemoryStatus checks the memory values from the device and logs them if they have changed
// or if a certain amount of time has passed.
func logMemoryStatus(data map[string]interface{}) {
	// Extract current values, using a helper to avoid panics
	getFloat := func(key string) float64 {
		if val, ok := data[key]; ok {
			if fVal, ok := val.(float64); ok {
				return fVal
			}
		}
		return 0
	}

	currentHeapFree := getFloat("hf")
	currentHeapMinFree := getFloat("hmf")
	currentHeapMaxAlloc := getFloat("hma")
	currentHeapSize := getFloat("hs")

	// Check if any value has changed
	valuesChanged := currentHeapFree != lastLoggedHeapFree ||
		currentHeapMinFree != lastLoggedHeapMinFree ||
		currentHeapMaxAlloc != lastLoggedHeapMaxAlloc ||
		currentHeapSize != lastLoggedHeapSize

	// Check if it's been more than 15 minutes since the last log
	timeForcedLog := time.Since(lastMemoryLogTime) > 2*time.Minute

	if valuesChanged || timeForcedLog {
		logDebug("ESP32 Heap Status: Size=%.0f, Free=%.0f, MinFree=%.0f, MaxAlloc=%.0f",
			currentHeapSize, currentHeapFree, currentHeapMinFree, currentHeapMaxAlloc)

		// Update the last logged values
		lastLoggedHeapFree = currentHeapFree
		lastLoggedHeapMinFree = currentHeapMinFree
		lastLoggedHeapMaxAlloc = currentHeapMaxAlloc
		lastLoggedHeapSize = currentHeapSize
		lastMemoryLogTime = time.Now()
	}
}

func manageConnection(initDone chan struct{}) {
	logInfo("Connection manager task started. Waiting for initial connection...")
	<-initDone // Wait for the initial connection to be established
	logInfo("Initial connection complete. Starting connection management.")

	for {
		// Wait before the next check. This also gives the initial startup process a chance to complete.
		time.Sleep(5 * time.Second)

		logDebug("Connection Manager: Checking connection status...")

		// We acquire the lock here to ensure that no other part of the application
		// (like the initial startup) can interfere with our connection check and attempt.
		portMutex.Lock()

		isConnected := (sv241Port != nil)

		if !isConnected {
			logInfo("Connection Manager: Device is disconnected. Attempting to connect...")
			targetPort := proxyConfig.SerialPortName

			if targetPort != "" {
				logInfo("Connection Manager: Trying configured port '%s' for reconnection.", targetPort)
				reconnectSerialPort(targetPort)

				if sv241Port != nil {
					logInfo("Connection Manager: Successfully reconnected to port '%s'.", targetPort)
					portMutex.Unlock() // Release lock and continue the loop
					continue
				}

				logWarn("Connection Manager: Configured port '%s' failed to reconnect. Falling back to auto-detection.", targetPort)
				proxyConfig.SerialPortName = ""
				saveProxyConfig()
			}

			logInfo("Connection Manager: Starting auto-detection...")
			foundPort, err := findSV241Port()
			if err != nil {
				logWarn("Connection Manager: Auto-detection failed: %v", err)
			} else {
				logInfo("Connection Manager: Auto-detection found device on port %s. Connecting...", foundPort)
				reconnectSerialPort(foundPort)
			}
		} else {
			logDebug("Connection Manager: Device is connected.")
		}

		portMutex.Unlock() // Release the lock after the check/connection attempt is complete.
	}
}

func respondToAlpacaDiscovery() {
	// Listen on all IPv4 interfaces for UDP packets on the Alpaca discovery port.
	addr, err := net.ResolveUDPAddr("udp4", "0.0.0.0:32227")
	if err != nil {
		logError("Discovery: Could not resolve UDP address: %v", err)
		return
	}

	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		logError("Discovery: Could not listen on UDP port 32227: %v", err)
		logInfo("HINT: This may be caused by another Alpaca application running, or a permissions issue.")
		return
	}
	defer conn.Close()
	logInfo("Alpaca discovery responder started on UDP port 32227.")

	discoveryMsg := []byte("alpacadiscovery1")
	buffer := make([]byte, 1024) // Buffer for incoming data

	for {
		n, remoteAddr, err := conn.ReadFromUDP(buffer)
		if err != nil {
			logWarn("Discovery: Error reading from UDP: %v", err)
			continue
		}

		// Check if the received message is the Alpaca discovery message
		if string(buffer[:n]) == string(discoveryMsg) {
			logDebug("Discovery: Request received from %s", remoteAddr)

			response := fmt.Sprintf(`{"AlpacaPort": %d}`, proxyConfig.NetworkPort)

			// Send the response back to the client who sent the request
			_, err := conn.WriteToUDP([]byte(response), remoteAddr)
			if err != nil {
				logError("Discovery: Failed to send response to %s: %v", remoteAddr, err)
			} else {
				logDebug("Discovery: Sent response '%s' to %s", response, remoteAddr)
			}
		}
	}
}

// --- Serial Communication ---
// sendCommandToDevice queues a command to be sent to the device.
// It accepts an optional timeout duration. If timeout is 0, a default is used.
func sendCommandToDevice(command string, isHighPriority bool, timeout time.Duration) (string, error) {
	if timeout == 0 {
		timeout = 3 * time.Second // Default timeout
	}

	responseChan := make(chan string, 1)
	errorChan := make(chan error, 1)

	cmd := SerialCommand{
		Command:  command,
		Response: responseChan,
		Error:    errorChan,
	}

	if isHighPriority {
		logDebug("Queueing high-priority command: %s", command)
		highPriorityCommands <- cmd
	} else {
		logDebug("Queueing low-priority command: %s", command)
		lowPriorityCommands <- cmd
	}

	// Wait for the response from the command processor
	select {
	case response := <-responseChan:
		return response, nil
	case err := <-errorChan:
		return "", err
	case <-time.After(timeout): // Use the specified or default timeout
		return "", errors.New("command timed out waiting for response from processor")
	}
}

// handleDisconnect closes the port and sets it to nil. MUST be called within a portMutex lock.
func handleDisconnect() {
	if sv241Port != nil {
		sv241Port.Close()
		sv241Port = nil
	}
}

// findSV241Port iterates synchronously through available serial ports, opens them one by one,
// sends a direct probe command, and returns the name of the first port that responds correctly.
// This function is self-contained and ensures the port is closed before returning its name to avoid race conditions.
func findSV241Port() (string, error) {
	ports, err := enumerator.GetDetailedPortsList()
	if err != nil {
		logWarn("findSV241Port: enumerator.GetDetailedPortsList returned an error: %v.", err)
	}

	if len(ports) == 0 {
		return "", errors.New("no serial ports found on the system")
	}

	logInfo("Found %d serial ports. Probing for SV241 device...", len(ports))
	for _, port := range ports {
		if port.IsUSB {
			logInfo("Probing port: %s", port.Name)

			mode := &serial.Mode{BaudRate: 115200}
			p, err := serial.Open(port.Name, mode)
			if err != nil {
				logWarn("Could not open port %s to probe: %v", port.Name, err)
				continue
			}

			// Send probe command
			_, err = p.Write([]byte("{\"get\":\"sensors\"}\n"))
			if err != nil {
				p.Close()
				continue
			}

			// Read response
			p.SetReadTimeout(2 * time.Second) // A slightly longer timeout might be safer
			reader := bufio.NewReader(p)
			line, err := reader.ReadString('\n')
			if err != nil {
				p.Close()
				continue
			}

			// We are done with the port, close it before continuing.
			p.Close()

			// Validate response
			var js json.RawMessage
			if json.Unmarshal([]byte(line), &js) == nil {
				logInfo("Successfully probed port: %s", port.Name)
				return port.Name, nil // Return the name of the now-closed port
			}
		}
	}
	return "", errors.New("could not find SV241 device on any USB serial port")
}

// openBrowser opens the specified URL in the default browser.
func openBrowser(url string) {
	var err error
	switch runtime.GOOS {
	case "linux":
		err = exec.Command("xdg-open", url).Start()
	case "windows":
		err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		err = exec.Command("open", url).Start()
	default:
		err = fmt.Errorf("unsupported platform")
	}
	if err != nil {
		logError("Failed to open browser: %v", err)
	}
}

// --- Alpaca HTTP Handlers ---

// Management and Setup
func handleManagementDescription(w http.ResponseWriter, r *http.Request) {
	description := AlpacaDescription{
		ServerName:          "SV241 Alpaca Proxy",
		Manufacturer:        "User-Made",
		ManufacturerVersion: "0.1.0",
		Location:            "My Observatory",
	}
	managementValueResponse(w, r, description)
}

func handleManagementConfiguredDevices(w http.ResponseWriter, r *http.Request) {
	devices := []AlpacaConfiguredDevice{
		{
			DeviceName:   "SV241 Power Switch",
			DeviceType:   "Switch",
			DeviceNumber: 0,
			UniqueID:     "a7f5a59c-f5d3-47f5-a59c-f5d347f5a59c", // Static GUID for conformance
		},
		{
			DeviceName:   "SV241 Environment",
			DeviceType:   "ObservingConditions",
			DeviceNumber: 0,
			UniqueID:     "b8g6b69d-g6e4-58g6-b69d-g6e458g6b69d", // Static GUID for conformance
		},
	}
	managementValueResponse(w, r, devices)
}

func handleManagementApiVersions(w http.ResponseWriter, r *http.Request) {
	// This endpoint is part of the management API and doesn't use the standard alpacaHandler wrapper.
	// We need to construct the full response structure manually.
	// ClientTransactionID and ServerTransactionID are often 0 for management discovery.
	response := struct {
		Value               []int  `json:"Value"`
		ClientTransactionID uint32 `json:"ClientTransactionID"`
		ServerTransactionID uint32 `json:"ServerTransactionID"`
		ErrorNumber         int    `json:"ErrorNumber"`
		ErrorMessage        string `json:"ErrorMessage"`
	}{
		Value:               []int{1},
		ClientTransactionID: 0, // Not available in this context
		ServerTransactionID: 0, // Not stateful for this endpoint
		ErrorNumber:         0,
		ErrorMessage:        "",
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func handleSetupPage(w http.ResponseWriter, r *http.Request) {
	// Setze den Pfad explizit auf den Dateinamen im embed.FS
	r.URL.Path = "setup.html"
	http.FileServer(http.FS(setupHTML)).ServeHTTP(w, r)
}

func handleGetConfig(w http.ResponseWriter, r *http.Request) {
	resp, err := sendCommandToDevice(`{"get":"config"}`, false, 0) // Low priority, default timeout
	if err != nil {
		alpacaErrorResponse(w, r, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, resp)
}

func handleSetConfig(w http.ResponseWriter, r *http.Request) {
	// The request body now comes from the UI with short keys.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	// Basic validation: check if it's a valid JSON
	var js json.RawMessage
	if json.Unmarshal(body, &js) != nil {
		http.Error(w, "Invalid JSON format", http.StatusBadRequest)
		return
	}

	command := fmt.Sprintf(`{"sc":%s}`, string(body))
	logDebug("Sending to device: %s", command)

	// The ESP32 firmware is designed to respond with the full, updated configuration
	// immediately after a `set_config` command. We capture this response directly
	// instead of sending a second `get:config` command. This prevents race conditions.
	shortKeyResponse, err := sendCommandToDevice(command, true, 10*time.Second) // High priority, longer timeout for flash write
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to send command to device: %v", err), http.StatusServiceUnavailable)
		return
	}

	// Forward the response (which has short keys) directly to the web UI.
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, shortKeyResponse)
}

// handleCreateBackup creates a combined backup of proxy and firmware settings.
func handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	logInfo("Creating combined configuration backup...")

	// 1. Get firmware config
	shortKeyFirmwareConfigJSON, err := sendCommandToDevice(`{"get":"config"}`, true, 0) // High priority, default timeout
	if err != nil {
		logError("Backup failed: could not get firmware config: %v", err)
		http.Error(w, "Failed to get firmware configuration from device", http.StatusInternalServerError)
		return
	}

	// 2. Create combined config
	// The firmware config is saved with its original short keys.
	backup := CombinedConfig{
		ProxyConfig:    proxyConfig,
		FirmwareConfig: json.RawMessage(shortKeyFirmwareConfigJSON),
	}

	// 3. Marshal to JSON
	backupJSON, err := json.MarshalIndent(backup, "", "  ") // Use MarshalIndent for readability
	if err != nil {
		logError("Backup failed: could not marshal combined config: %v", err)
		http.Error(w, "Failed to create backup file", http.StatusInternalServerError)
		return
	}

	// 4. Send as file download
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="sv241_backup.json"`)
	w.Write(backupJSON)
	logInfo("Successfully created and sent configuration backup.")
}

// handleRestoreBackup restores a combined backup of proxy and firmware settings.
func handleRestoreBackup(w http.ResponseWriter, r *http.Request) {
	logInfo("Restoring combined configuration from backup...")

	// 1. Read and parse the uploaded file
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var backup CombinedConfig
	if err := json.Unmarshal(body, &backup); err != nil {
		logError("Restore failed: could not unmarshal backup file: %v", err)
		http.Error(w, "Invalid backup file format", http.StatusBadRequest)
		return
	}

	// 2. Validate the backup file
	if backup.ProxyConfig == nil || backup.FirmwareConfig == nil {
		logError("Restore failed: backup file is missing required sections.")
		http.Error(w, "Incomplete backup file: missing proxy or firmware config", http.StatusBadRequest)
		return
	}

	// 3. Restore Firmware Config
	logInfo("Restoring firmware configuration...")
	// We need to re-marshal the firmware config to ensure it's a single, compact line of JSON,
	// as the backup file might have it indented.
	var firmwareConfigMap map[string]interface{}
	if err := json.Unmarshal(backup.FirmwareConfig, &firmwareConfigMap); err != nil {
		logError("Restore failed: could not parse firmware config from backup: %v", err)
		http.Error(w, "Invalid firmware configuration in backup file", http.StatusBadRequest)
		return
	}
	compactFirmwareConfig, _ := json.Marshal(firmwareConfigMap) // Marshal without indentation

	firmwareCommand := fmt.Sprintf(`{"sc":%s}`, string(compactFirmwareConfig))

	if _, err := sendCommandToDevice(firmwareCommand, true, 10*time.Second); err != nil { // High priority with longer timeout
		logError("Restore failed: could not set firmware config: %v", err)
		http.Error(w, fmt.Sprintf("Failed to send firmware configuration to device: %v", err), http.StatusServiceUnavailable)
		return
	}
	logInfo("Firmware configuration restored successfully.")

	// 4. Restore Proxy Config
	logInfo("Restoring proxy configuration...")
	// Update the global proxyConfig object with the data from the backup
	proxyConfig.NetworkPort = backup.ProxyConfig.NetworkPort
	proxyConfig.LogLevel = backup.ProxyConfig.LogLevel
	proxyConfig.SwitchNames = backup.ProxyConfig.SwitchNames

	// *** INTELLIGENT RESTORE: Clear the serial port name to trigger auto-detection ***
	proxyConfig.SerialPortName = ""
	logInfo("Serial port name has been cleared to trigger auto-detection on the new system.")

	setLogLevelFromString(proxyConfig.LogLevel)
	logInfo("Log level set to %s", proxyConfig.LogLevel)

	if err := saveProxyConfig(); err != nil {
		logError("Restore failed: could not save proxy config: %v", err)
		http.Error(w, "Failed to save proxy configuration", http.StatusInternalServerError)
		return
	}
	logInfo("Proxy configuration restored successfully.")

	// The serial port might have changed, so we trigger a reconnect attempt.
	go func() {
		portMutex.Lock()
		defer portMutex.Unlock()
		reconnectSerialPort(proxyConfig.SerialPortName)
	}()

	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "Configuration restored successfully. Please allow a moment for all settings to apply.")
}

// handleGetPowerStatus returns the current power status from the cache.
func handleGetPowerStatus(w http.ResponseWriter, r *http.Request) {
	statusCache.RLock()
	defer statusCache.RUnlock()

	if statusCache.Data == nil {
		http.Error(w, "Status cache is not yet populated", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(statusCache.Data)
}

// handleSetAllPower sets the state of all power outputs.
func handleSetAllPower(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		State bool `json:"state"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	stateInt := 0
	if payload.State {
		stateInt = 1
	}
	// Translate "all" to the short key if needed, though "all" is a special case and might not need translation
	command := fmt.Sprintf(`{"set":{"all":%d}}`, stateInt)
	responseJSON, err := sendCommandToDevice(command, true, 0) // High priority, default timeout
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to send command to device: %v", err), http.StatusServiceUnavailable)
		return
	}

	// The firmware responds with short keys. We need to translate them back to long keys for the cache.
	var shortKeyStatus map[string]map[string]interface{}
	if json.Unmarshal([]byte(responseJSON), &shortKeyStatus) == nil {
		// This is a simplified translation. A full implementation would iterate and map.
		// For now, we'll just update the cache with the raw (short-keyed) data and let the next periodic update fix it.
		statusCache.Lock()
		statusCache.Data = shortKeyStatus["status"]
		statusCache.Unlock()
	}
	var statusData map[string]map[string]interface{}
	if json.Unmarshal([]byte(responseJSON), &statusData) == nil {
		statusCache.Lock()
		statusCache.Data = statusData["status"]
		statusCache.Unlock()
	} else {
		logWarn("Failed to unmarshal status JSON from device after set all command. Raw data: %s", responseJSON)
	}

	w.WriteHeader(http.StatusOK)
}

// handleGetLiveStatus returns the current live status from the conditions cache.
func handleGetLiveStatus(w http.ResponseWriter, r *http.Request) {
	conditionsCache.RLock()
	defer conditionsCache.RUnlock()

	if conditionsCache.Data == nil {
		// Return an empty JSON object if the cache is not populated yet.
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "{}")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(conditionsCache.Data)
}

// handleGetProxyConfig returns the proxy's configuration.
func handleGetProxyConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(proxyConfig)
}

// handleSetProxyConfig updates and saves the proxy's configuration.
func handleSetProxyConfig(w http.ResponseWriter, r *http.Request) {
	var newProxyConfig ProxyConfig
	logInfo("Received request to set new proxy config...")
	if err := json.NewDecoder(r.Body).Decode(&newProxyConfig); err != nil {
		logError("Failed to decode new proxy config JSON: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	logDebug("Successfully decoded new proxy config.") // Don't log the full struct here, it's redundant with the load log.

	// Update the global proxyConfig
	proxyConfig.SerialPortName = newProxyConfig.SerialPortName
	proxyConfig.NetworkPort = newProxyConfig.NetworkPort
	proxyConfig.SwitchNames = newProxyConfig.SwitchNames // This line was missing
	proxyConfig.LogLevel = newProxyConfig.LogLevel
	proxyConfig.HeaterAutoEnableLeader = newProxyConfig.HeaterAutoEnableLeader

	setLogLevelFromString(proxyConfig.LogLevel)
	logInfo("Log level set to %s", proxyConfig.LogLevel)

	if err := saveProxyConfig(); err != nil {
		logError("handleSetProxyConfig: Failed to save proxy config: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Hinweis für den Benutzer, dass ein Neustart für die Port-Änderung erforderlich ist.
	// Die eigentliche Port-Änderung wird erst beim nächsten Start der Anwendung wirksam.
	// Wir könnten hier eine komplexere Logik für einen Live-Neustart des Servers implementieren,
	// aber ein einfacher Hinweis ist für den Anfang ausreichend und sicherer.

	// Perform the reconnect in a separate goroutine to avoid blocking the HTTP response.
	// This also prevents deadlocks with the manageConnection goroutine.
	logDebug("handleSetProxyConfig: Reconnection goroutine started. Attempting to acquire portMutex...")
	go func() {
		portMutex.Lock()
		defer portMutex.Unlock()
		logDebug("handleSetProxyConfig: portMutex acquired. Calling reconnectSerialPort with '%s'.", proxyConfig.SerialPortName)
		reconnectSerialPort(proxyConfig.SerialPortName)
		logDebug("handleSetProxyConfig: reconnectSerialPort finished. Releasing portMutex.")
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(proxyConfig) // Return the updated config
}

// handleDeviceCommand receives a generic JSON command from the web UI and passes it to the device.
func handleDeviceCommand(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Invalid request method", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// Basic validation: check if it's a valid JSON
	var js json.RawMessage
	if json.Unmarshal(body, &js) != nil {
		http.Error(w, "Invalid JSON in request body", http.StatusBadRequest)
		return
	}

	command := string(body)

	// Special handling for the reboot command
	if command == `{"command":"reboot"}` || command == `{"command":"factory_reset"}` {
		logInfo("Received command '%s' from web UI. Sending to device and closing port.", command)
		portMutex.Lock()
		if sv241Port != nil {
			sv241Port.Write([]byte(command + "\n")) // Send command without waiting for response
			handleDisconnect()                      // Proactively close the port
		}
		portMutex.Unlock()
	} else {
		logDebug("Received generic command from web UI: %s", command)
		if _, err := sendCommandToDevice(command, true, 0); err != nil { // High priority, default timeout
			http.Error(w, fmt.Sprintf("Failed to send command to device: %v", err), http.StatusServiceUnavailable)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "Command sent successfully.")
}

func handleGetFirmwareVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	response := struct {
		Version string `json:"version"`
	}{
		Version: firmwareVersion,
	}
	json.NewEncoder(w).Encode(response)
}

// loadProxyConfig loads the proxy's configuration from a JSON file.
func loadProxyConfig() error {
	file, err := os.ReadFile(proxyConfigFile)
	if err != nil {
		if os.IsNotExist(err) {
			logInfo("Proxy config file '%s' not found. Using default settings.", proxyConfigFile)
			// Initialize with default values
			proxyConfig.NetworkPort = 8080 // Set default port if file doesn't exist
			proxyConfig.LogLevel = "INFO"
			proxyConfig.SwitchNames = make(map[string]string)
			proxyConfig.HeaterAutoEnableLeader = map[string]bool{
				"pwm1": true,
				"pwm2": true,
			}
			for _, internalName := range switchIDMap {
				proxyConfig.SwitchNames[internalName] = internalName
			}
			// Attempt to save the initial default config
			saveProxyConfig()
			return nil // File not found is not an error, just means defaults apply
		}
		return fmt.Errorf("failed to read proxy config file: %w", err)
	}

	if err := json.Unmarshal(file, proxyConfig); err != nil {
		proxyConfig.NetworkPort = 8080 // Fallback to default port on unmarshal error
		proxyConfig.LogLevel = "INFO"
		proxyConfig.SwitchNames = make(map[string]string) // Fallback
		return fmt.Errorf("failed to unmarshal proxy config: %w", err)
	}

	// Ensure a default port is set if it's missing or zero from the config file
	if proxyConfig.NetworkPort == 0 {
		proxyConfig.NetworkPort = 8080
	}
	// Ensure a default log level is set
	if proxyConfig.LogLevel == "" {
		logWarn("Configuration key 'LogLevel' not found, using default 'INFO'.")
		proxyConfig.LogLevel = "INFO" // Set the default
	}
	// Ensure the switch names map exists and is populated for any new switches
	if proxyConfig.SwitchNames == nil {
		proxyConfig.SwitchNames = make(map[string]string)
	}
	for _, internalName := range switchIDMap {
		if _, exists := proxyConfig.SwitchNames[internalName]; !exists {
			logWarn("Missing custom name for '%s', adding with default value.", internalName)
			proxyConfig.SwitchNames[internalName] = internalName
		}
	}
	// Ensure the auto-enable map exists and is populated for both heaters
	if proxyConfig.HeaterAutoEnableLeader == nil {
		proxyConfig.HeaterAutoEnableLeader = make(map[string]bool)
	}
	if _, exists := proxyConfig.HeaterAutoEnableLeader["pwm1"]; !exists {
		logWarn("Missing auto-enable setting for 'pwm1', adding with default 'true'.")
		proxyConfig.HeaterAutoEnableLeader["pwm1"] = true
	}
	if _, exists := proxyConfig.HeaterAutoEnableLeader["pwm2"]; !exists {
		logWarn("Missing auto-enable setting for 'pwm2', adding with default 'true'.")
		proxyConfig.HeaterAutoEnableLeader["pwm2"] = true
	}

	// Apply the loaded log level immediately.
	setLogLevelFromString(proxyConfig.LogLevel)
	logInfo("Loaded proxy config: %+v", proxyConfig)
	return nil
}

// saveProxyConfig saves the proxy's configuration to a JSON file.
func saveProxyConfig() error {
	logDebug("Attempting to save proxy config: %+v", proxyConfig)
	data, err := json.MarshalIndent(proxyConfig, "", "  ")
	if err != nil {
		logError("saveProxyConfig: failed to marshal proxy config: %v", err)
		return fmt.Errorf("failed to marshal proxy config: %w", err)
	}

	logDebug("Attempting to write proxy config to file: %s", proxyConfigFile)
	if err := os.WriteFile(proxyConfigFile, data, 0644); err != nil {
		logError("saveProxyConfig: failed to write proxy config file '%s': %v", proxyConfigFile, err)
		return fmt.Errorf("failed to write proxy config file: %w", err)
	}
	logInfo("Successfully saved proxy config to file '%s'", proxyConfigFile)
	return nil
}

// setLogLevelFromString updates the global currentLogLevel based on a string value.
func setLogLevelFromString(level string) {
	switch strings.ToUpper(level) {
	case "DEBUG":
		currentLogLevel = LogLevelDebug
	case "WARN":
		currentLogLevel = LogLevelWarn
	case "ERROR":
		currentLogLevel = LogLevelError
	default: // "INFO" and any other value
		currentLogLevel = LogLevelInfo
	}
}

// --- Log Level Helpers ---
func logError(format string, v ...interface{}) {
	if currentLogLevel >= LogLevelError {
		log.Printf("[ERROR] "+format, v...)
	}
}

func logWarn(format string, v ...interface{}) {
	if currentLogLevel >= LogLevelWarn {
		log.Printf("[WARN] "+format, v...)
	}
}

func logInfo(format string, v ...interface{}) {
	if currentLogLevel >= LogLevelInfo {
		log.Printf("[INFO] "+format, v...)
	}
}

func logDebug(format string, v ...interface{}) {
	if currentLogLevel >= LogLevelDebug {
		log.Printf("[DEBUG] "+format, v...)
	}
}

// logAndExit logs a fatal error and ensures the log file is closed before exiting.
// This replaces log.Fatalf to prevent empty log files on abrupt termination.
func logAndExit(format string, v ...interface{}) {
	// Use fmt.Fprintf to write directly to the file handle, bypassing the log buffer.
	// This ensures the final message is written even if the logger is misconfigured.
	fmt.Fprintf(logFile, "[FATAL] "+format+"\n", v...)
	if logFile != nil {
		logFile.Sync()  // Force flush
		logFile.Close() // Then close
	}
	os.Exit(1)
}

// reconnectSerialPort attempts to close the current port and open a new one.
// It MUST be called within a portMutex lock.
func reconnectSerialPort(newPortName string) {
	if sv241Port != nil {
		logInfo("Closing existing serial port...")
		sv241Port.Close()
		sv241Port = nil
	}

	if newPortName != "" {
		logInfo("Attempting to open configured serial port: %s", newPortName)
		mode := &serial.Mode{BaudRate: 115200}
		p, err := serial.Open(newPortName, mode)
		if err != nil {
			logError("reconnectSerialPort: Failed to open port %s: %v", newPortName, err)
		} else {
			logInfo("Successfully opened serial port: %s", newPortName)
			sv241Port = p
			proxyConfig.SerialPortName = newPortName // Update config with the valid port
			if err := saveProxyConfig(); err != nil {
				logWarn("Failed to save newly connected serial port to config: %v", err)
			}
			logInfo("Successfully opened configured serial port: %s", newPortName)
		}
	} else {
		logInfo("reconnectSerialPort called with empty port name. Connection remains closed.")
		// This case is hit when the user clears the manual port config.
		// The manageConnection loop will then trigger auto-detection.
	}
}

// Common Device Handlers
func handleDeviceDescription(w http.ResponseWriter, r *http.Request) {
	alpacaStringResponse(w, r, "SV241 Pro Proxy Driver")
}

func handleDriverInfo(w http.ResponseWriter, r *http.Request) {
	alpacaStringResponse(w, r, "A Go-based ASCOM Alpaca proxy driver for the SV241 Pro.")
}

func handleDriverVersion(w http.ResponseWriter, r *http.Request) {
	alpacaStringResponse(w, r, "0.1.0")
}

func handleInterfaceVersion(w http.ResponseWriter, r *http.Request) {
	alpacaIntResponse(w, r, 1)
}

func handleConnected(w http.ResponseWriter, r *http.Request) {
	if r.Method == "PUT" {
		// The client is telling us to connect or disconnect.
		// In this proxy model, the connection is managed automatically in the background.
		// We don't need to do anything here, just acknowledge the request.
		connectedStr, ok := getFormValueIgnoreCase(r, "Connected")
		if !ok {
			alpacaErrorResponse(w, r, 0x400, "Missing Connected parameter for PUT request")
			return
		}
		logDebug("Received PUT Connected request with Connected=%s. Acknowledging request without action.", connectedStr)
		// Conformity check: validate the "Connected" parameter
		if _, err := strconv.ParseBool(connectedStr); err != nil {
			// According to spec, if the value is not a valid boolean, return an error.
			alpacaErrorResponse(w, r, 0x400, fmt.Sprintf("Invalid value for Connected: '%s'", connectedStr))
			return
		}
		alpacaEmptyResponse(w, r) // Acknowledge valid request
		return
	}

	// Default to GET behavior
	alpacaBoolResponse(w, r, sv241Port != nil)
}

func handleDeviceName(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		alpacaStringResponse(w, r, name)
	}
}

func handleSupportedActions(w http.ResponseWriter, r *http.Request) {
	// Return an empty list of actions, which is valid.
	alpacaStringListResponse(w, r, []string{})
}

// Switch Handlers
func handleSwitchMaxSwitch(w http.ResponseWriter, r *http.Request) {
	alpacaIntResponse(w, r, len(switchIDMap))
}

func handleSwitchGetSwitchName(w http.ResponseWriter, r *http.Request) {
	if id, ok := parseSwitchID(w, r); ok {
		internalName := switchIDMap[id]
		// Check if a custom name exists in the proxy config, otherwise use the internal name.
		customName, nameExists := proxyConfig.SwitchNames[internalName]
		if nameExists && customName != "" {
			alpacaStringResponse(w, r, customName)
		} else {
			alpacaStringResponse(w, r, internalName)
		}
	}
}

func handleSwitchGetSwitchDescription(w http.ResponseWriter, r *http.Request) {
	if id, ok := parseSwitchID(w, r); ok {
		// The description should be the non-changeable, internal system name.
		internalName := switchIDMap[id]
		alpacaStringResponse(w, r, internalName)
	}
}

func handleSwitchGetSwitch(w http.ResponseWriter, r *http.Request) {
	id, ok := parseSwitchID(w, r)
	if !ok {
		return // Error already sent
	}
	shortKey := shortSwitchKeyByID[id]
	statusCache.RLock()
	defer statusCache.RUnlock()
	if val, ok := statusCache.Data[shortKey]; ok {
		// The value from the device is 1.0 for on, 0.0 for off.
		alpacaBoolResponse(w, r, val.(float64) >= 1.0)
	} else {
		alpacaErrorResponse(w, r, http.StatusInternalServerError, "Could not read switch status from cache")
	}
}

func handleSwitchGetSwitchValue(w http.ResponseWriter, r *http.Request) {
	id, ok := parseSwitchID(w, r)
	if !ok {
		return // Error already sent
	}
	shortKey := shortSwitchKeyByID[id]
	statusCache.RLock()
	defer statusCache.RUnlock()
	if val, ok := statusCache.Data[shortKey]; ok {
		// For a boolean switch, ASCOM expects 1.0 for on and 0.0 for off.
		var switchValue float64
		if val.(float64) >= 1.0 {
			switchValue = 1.0
		} else {
			switchValue = 0.0
		}
		alpacaFloatResponse(w, r, switchValue)
	} else {
		alpacaErrorResponse(w, r, http.StatusInternalServerError, "Could not read switch status from cache")
	}
}

func handleSwitchSetSwitchValue(w http.ResponseWriter, r *http.Request) {
	id, ok := parseSwitchID(w, r)
	if !ok {
		return // Error already sent by parseSwitchID
	}

	var state bool
	var err error
	// NINA uses SetSwitchValue which sends a "Value" parameter (0.0 or 1.0)
	// Other clients might use SetSwitch which sends a "State" parameter (true/false)
	// We need to handle both, case-insensitively.
	if valueStr, ok := getFormValueIgnoreCase(r, "Value"); ok {
		value, err := strconv.ParseFloat(valueStr, 64)
		if err != nil {
			alpacaErrorResponse(w, r, 400, "Invalid Value parameter")
			return
		}
		state = (value >= 1.0)
	} else if stateStr, ok := getFormValueIgnoreCase(r, "State"); ok {
		state, err = strconv.ParseBool(stateStr)
		if err != nil {
			alpacaErrorResponse(w, r, 400, "Invalid State parameter")
			return
		}
	} else {
		alpacaErrorResponse(w, r, 400, "Missing Value or State parameter")
		return
	}

	longKey := switchIDMap[id]
	shortKey, ok := shortSwitchIDMap[longKey]
	if !ok {
		shortKey = longKey // Fallback to long key if not in map
	}
	stateInt := 0
	if state {
		stateInt = 1
	}
	command := fmt.Sprintf(`{"set":{"%s":%d}}`, shortKey, stateInt)
	responseJSON, err := sendCommandToDevice(command, true, 0) // High priority, default timeout
	if err != nil {
		alpacaErrorResponse(w, r, http.StatusInternalServerError, fmt.Sprintf("Failed to send command: %v", err))
		return
	}

	// The firmware now responds with the full status. We can use this to update the cache directly.
	var statusData map[string]map[string]interface{}
	if json.Unmarshal([]byte(responseJSON), &statusData) == nil {
		statusCache.Lock()
		statusCache.Data = statusData["status"]
		statusCache.Unlock()
	} else {
		logWarn("Failed to unmarshal status JSON from device after set command. Raw data: %s", responseJSON)
		// If parsing fails, fall back to the periodic update; no need to force one.
	}

	// --- Auto-Enable PID Leader Logic ---
	// This logic checks if a newly activated switch is a "Follower" heater
	// and if its configuration dictates that its "Leader" should be auto-enabled.
	if state { // Only act when a switch is turned ON
		// Check if the switch is one of the heaters (ASCOM ID 8 or 9)
		if id == 8 || id == 9 {
			// Run in a separate goroutine to avoid blocking the response to the client.
			go func(followerId int) {
				followerHeaterIndex := followerId - 8 // Map ASCOM ID (8,9) to heater index (0,1)
				followerKey := fmt.Sprintf("pwm%d", followerHeaterIndex+1)

				// 1. Check if the feature is enabled for this specific heater in the proxy config.
				if !proxyConfig.HeaterAutoEnableLeader[followerKey] {
					logDebug("Auto-enable leader is disabled for %s in proxy config. Skipping.", followerKey)
					return
				}
				logDebug("Heater %s (ID %d) turned on. Checking for auto-enable leader logic.", followerKey, followerId)

				// 2. Get the latest firmware config to check modes.
				configJSON, err := sendCommandToDevice(`{"get":"config"}`, false, 0)
				if err != nil {
					logWarn("Auto-Enable: Could not get firmware config: %v", err)
					return
				}

				// Define a struct that matches the firmware's JSON response for dew heaters.
				var fwConfig struct {
					DH []struct {
						M int `json:"m"` // Mode (0:Manual, 1:PID, 2:Ambient, 3:Follower)
					} `json:"dh"`
					// Add other top-level keys from the firmware config to make parsing robust
					// We only need 'dh' for this logic, but this prevents unmarshal errors.
				}

				if err := json.Unmarshal([]byte(configJSON), &fwConfig); err != nil {
					logWarn("Auto-Enable: Could not parse firmware config: %v", err)
					return
				}

				// 3. Determine leader index and check conditions.
				leaderHeaterIndex := 1 - followerHeaterIndex
				leaderAscomId := leaderHeaterIndex + 8

				isFollower := fwConfig.DH[followerHeaterIndex].M == 3
				isLeaderPID := fwConfig.DH[leaderHeaterIndex].M == 1

				if isFollower && isLeaderPID {
					logInfo("Activating PID Leader (ID %d) for Follower (ID %d) as per proxy configuration.", leaderAscomId, followerId)
					leaderShortKey := shortSwitchKeyByID[leaderAscomId]
					leaderCommand := fmt.Sprintf(`{"set":{"%s":1}}`, leaderShortKey)
					if _, err := sendCommandToDevice(leaderCommand, true, 0); err != nil {
						logError("Auto-Enable: Failed to send command to enable leader: %v", err)
					}
				}
			}(id)
		}
	}

	// --- Auto-Disable PID Follower Logic ---
	// This logic checks if a switch being turned OFF is a PID Leader,
	// and if so, it also turns off its Follower.
	if !state { // Only act when a switch is turned OFF
		// Check if the switch is one of the heaters (ASCOM ID 8 or 9)
		if id == 8 || id == 9 {
			// Run in a separate goroutine to avoid blocking the response to the client.
			go func(leaderId int) {
				logDebug("Heater (ID %d) turned off. Checking for auto-disable follower logic.", leaderId)

				// 1. Get the latest firmware config to check modes.
				configJSON, err := sendCommandToDevice(`{"get":"config"}`, false, 0)
				if err != nil {
					logWarn("Auto-Disable: Could not get firmware config: %v", err)
					return
				}

				var fwConfig struct {
					DH []struct {
						M int `json:"m"`
					} `json:"dh"`
				}
				if err := json.Unmarshal([]byte(configJSON), &fwConfig); err != nil {
					logWarn("Auto-Disable: Could not parse firmware config: %v", err)
					return
				}

				// 2. Determine indices for leader and follower.
				leaderHeaterIndex := leaderId - 8 // Map ASCOM ID (8,9) to heater index (0,1)
				followerHeaterIndex := 1 - leaderHeaterIndex
				followerAscomId := followerHeaterIndex + 8

				isLeaderPID := fwConfig.DH[leaderHeaterIndex].M == 1
				isFollower := fwConfig.DH[followerHeaterIndex].M == 3

				if isLeaderPID && isFollower {
					logInfo("Deactivating PID Follower (ID %d) because its Leader (ID %d) was turned off.", followerAscomId, leaderId)
					followerShortKey := shortSwitchKeyByID[followerAscomId]
					followerCommand := fmt.Sprintf(`{"set":{"%s":0}}`, followerShortKey)
					if _, err := sendCommandToDevice(followerCommand, true, 0); err != nil {
						logError("Auto-Disable: Failed to send command to disable follower: %v", err)
					}
				}
			}(id)
		}
	}

	alpacaEmptyResponse(w, r)
}

func handleSwitchSetSwitchName(w http.ResponseWriter, r *http.Request) {
	id, ok := parseSwitchID(w, r)
	if !ok {
		return // parseSwitchID already sent the error response for bad ID.
	}

	newName, ok := getFormValueIgnoreCase(r, "Name")
	if !ok {
		alpacaErrorResponse(w, r, http.StatusBadRequest, "Missing Name parameter")
		return
	}

	internalName := switchIDMap[id]

	// Ensure the SwitchNames map is initialized
	if proxyConfig.SwitchNames == nil {
		proxyConfig.SwitchNames = make(map[string]string)
	}

	// Update the name in the configuration
	proxyConfig.SwitchNames[internalName] = newName
	logInfo("Set custom name for switch %d ('%s') to '%s'", id, internalName, newName)

	// Save the updated configuration to the file
	if err := saveProxyConfig(); err != nil {
		log.Printf("ERROR: Failed to save proxy config after setting switch name: %v", err)
		alpacaErrorResponse(w, r, http.StatusInternalServerError, "Failed to save configuration")
		return
	}

	alpacaEmptyResponse(w, r)
}

func handleSwitchCanWrite(w http.ResponseWriter, r *http.Request) {
	if _, ok := parseSwitchID(w, r); ok {
		// All switches in this implementation are writable.
		alpacaBoolResponse(w, r, true)
	}
}

func handleSwitchMaxSwitchValue(w http.ResponseWriter, r *http.Request) {
	if _, ok := parseSwitchID(w, r); ok {
		// All switches in this implementation are boolean, so max value is 1.0.
		alpacaFloatResponse(w, r, 1.0)
	}
}

func handleSwitchMinSwitchValue(w http.ResponseWriter, r *http.Request) {
	if _, ok := parseSwitchID(w, r); ok {
		// All switches in this implementation are boolean, so min value is 0.0.
		alpacaFloatResponse(w, r, 0.0)
	}
}

func handleSwitchSwitchStep(w http.ResponseWriter, r *http.Request) {
	if _, ok := parseSwitchID(w, r); ok {
		// All switches in this implementation are boolean, so step is 1.0.
		alpacaFloatResponse(w, r, 1.0)
	}
}

func handleSwitchSupportedActions(w http.ResponseWriter, r *http.Request) {
	actions := []string{
		"getvoltage",
		"getcurrent",
		"getpower",
		"MasterSwitchOn",
		"MasterSwitchOff",
	}
	alpacaStringListResponse(w, r, actions)
}

func handleSwitchAction(w http.ResponseWriter, r *http.Request) {
	action, ok := getFormValueIgnoreCase(r, "Action")
	if !ok {
		alpacaErrorResponse(w, r, 0x400, "Missing Action parameter")
		return
	}

	var valueStr string

	switch strings.ToLower(action) {
	case "masterswitchon", "masterswitchoff":
		state := strings.ToLower(action) == "masterswitchon"
		logInfo("Executing ASCOM Action: %s", action)

		// Immediately acknowledge the action
		alpacaEmptyResponse(w, r)

		// Run in a goroutine to avoid blocking the response.
		go func() {
			stateInt := 0
			if state {
				stateInt = 1
			}
			command := fmt.Sprintf(`{"set":{"all":%d}}`, stateInt)
			responseJSON, err := sendCommandToDevice(command, true, 0) // High priority, default timeout
			if err != nil {
				logWarn("MasterSwitch Action: Failed to set all state: %v", err)
				return
			}
			// The firmware now responds with the full status. We can use this to update the cache directly.
			var statusData map[string]map[string]interface{}
			if json.Unmarshal([]byte(responseJSON), &statusData) == nil {
				statusCache.Lock()
				statusCache.Data = statusData["status"]
				statusCache.Unlock()
			} else {
				logWarn("Failed to unmarshal status JSON from device after master switch action. Raw data: %s", responseJSON)
			}
		}()
		return
	case "getvoltage":
		conditionsCache.RLock()
		defer conditionsCache.RUnlock()
		if value, found := conditionsCache.Data["v"]; found && value != nil {
			valueStr = fmt.Sprintf("%v", value)
		} else {
			alpacaErrorResponse(w, r, 0x500, "Value for action 'getvoltage' not available in cache.")
			return
		}
	case "getpower":
		conditionsCache.RLock()
		defer conditionsCache.RUnlock()
		if value, found := conditionsCache.Data["p"]; found && value != nil {
			valueStr = fmt.Sprintf("%v", value)
		} else {
			alpacaErrorResponse(w, r, 0x500, "Value for action 'getpower' not available in cache.")
			return
		}
	case "getcurrent":
		conditionsCache.RLock()
		defer conditionsCache.RUnlock()
		if value, found := conditionsCache.Data["i"]; found && value != nil {
			if currentMA, ok := value.(float64); ok {
				currentA := currentMA / 1000.0
				valueStr = fmt.Sprintf("%.3f", currentA) // Format to 3 decimal places for Amps
			} else {
				alpacaErrorResponse(w, r, 0x500, "Invalid data type for current in cache.")
				return
			}
		} else {
			alpacaErrorResponse(w, r, 0x500, "Value for action 'getcurrent' not available in cache.")
			return
		}
	default:
		alpacaErrorResponse(w, r, 0x400, fmt.Sprintf("Action '%s' is not supported.", action))
		return
	}

	alpacaStringResponse(w, r, valueStr)
}

// ObservingConditions Handlers
func handleObsCondTemperature(w http.ResponseWriter, r *http.Request) {
	conditionsCache.RLock()
	defer conditionsCache.RUnlock()
	if val, ok := conditionsCache.Data["t_amb"]; ok {
		alpacaFloatResponse(w, r, val.(float64))
	} else {
		alpacaErrorResponse(w, r, http.StatusInternalServerError, "Could not read temperature from cache")
	}
}

func handleObsCondHumidity(w http.ResponseWriter, r *http.Request) {
	conditionsCache.RLock()
	defer conditionsCache.RUnlock()
	if val, ok := conditionsCache.Data["h_amb"]; ok {
		alpacaFloatResponse(w, r, val.(float64))
	} else {
		alpacaErrorResponse(w, r, http.StatusInternalServerError, "Could not read humidity from cache")
	}
}

func handleObsCondDewPoint(w http.ResponseWriter, r *http.Request) {
	conditionsCache.RLock()
	defer conditionsCache.RUnlock()
	if val, ok := conditionsCache.Data["d"]; ok {
		alpacaFloatResponse(w, r, val.(float64))
	} else {
		alpacaErrorResponse(w, r, http.StatusInternalServerError, "Could not read dew point from cache")
	}
}

func handleObsCondNotImplemented(w http.ResponseWriter, r *http.Request) {
	// For properties that are not supported by the SV241 device.
	// According to Alpaca spec, we should return a NotImplemented error.
	alpacaErrorResponse(w, r, 0x40C, "Property not implemented by this driver.")
}

// handleObsCondAveragePeriod handles GET and PUT for the AveragePeriod property.
// Although not implemented, it must validate parameters for conformance.
func handleObsCondAveragePeriod(w http.ResponseWriter, r *http.Request) {
	if r.Method == "PUT" {
		// For PUT, we must validate the 'AveragePeriod' parameter even if we don't use it.
		// The conformance checker expects case-sensitivity for PUT parameters.
		avgPeriodStr, ok := getFormValueIgnoreCase(r, "AveragePeriod")
		if !ok {
			alpacaErrorResponse(w, r, 0x400, "Missing required parameter 'AveragePeriod' for PUT request.")
			return
		}
		// Check if the value is a valid float.
		if _, err := strconv.ParseFloat(avgPeriodStr, 64); err != nil {
			alpacaErrorResponse(w, r, 0x401, fmt.Sprintf("Invalid value '%s' for AveragePeriod.", avgPeriodStr))
			return
		}
		// If validation passes, we still return NotImplemented because we don't support setting it.
		alpacaErrorResponse(w, r, 0x40C, "Property not implemented by this driver.")
		return
	}

	// For GET, we just return that the property is not implemented.
	alpacaErrorResponse(w, r, 0x40C, "Property not implemented by this driver.")
}

// handleObsCondSensorDescription handles GET for the SensorDescription property.
// Although not implemented, it must validate parameters for conformance.
func handleObsCondSensorDescription(w http.ResponseWriter, r *http.Request) {
	// This is a GET-only property.
	if r.Method == "PUT" {
		alpacaErrorResponse(w, r, 0x405, "Method PUT not allowed for sensordescription.")
		return
	}

	// Validate the 'SensorName' parameter.
	sensorName, ok := getFormValueIgnoreCase(r, "SensorName")
	if !ok {
		alpacaErrorResponse(w, r, 0x400, "Missing required parameter 'SensorName'.")
		return
	}

	// The only valid sensor names for this device are "Temperature", "Humidity", and "DewPoint".
	// The conformance checker will test with invalid names.
	switch strings.ToLower(sensorName) {
	case "temperature", "humidity", "dewpoint":
		// If the name is valid, we still return NotImplemented because we don't have descriptions.
		alpacaErrorResponse(w, r, 0x40C, "Property not implemented by this driver.")
	default:
		// If the name is invalid, return an InvalidValue error.
		alpacaErrorResponse(w, r, 0x401, fmt.Sprintf("Invalid SensorName: '%s'", sensorName))
	}
}

// handleObsCondTimeSinceLastUpdate handles GET for the TimeSinceLastUpdate property.
// Although not implemented, it must validate parameters for conformance.
func handleObsCondTimeSinceLastUpdate(w http.ResponseWriter, r *http.Request) {
	// This is a GET-only property.
	if r.Method == "PUT" {
		alpacaErrorResponse(w, r, 0x405, "Method PUT not allowed for timesincelastupdate.")
		return
	}

	// Validate the 'SensorName' parameter.
	sensorName, ok := getFormValueIgnoreCase(r, "SensorName")
	if !ok {
		alpacaErrorResponse(w, r, 0x400, "Missing required parameter 'SensorName'.")
		return
	}

	// The only valid sensor names for this device are "Temperature", "Humidity", and "DewPoint".
	// The conformance checker will test with invalid names.
	switch strings.ToLower(sensorName) {
	case "temperature", "humidity", "dewpoint":
		// If the name is valid, we still return NotImplemented because we don't track this.
		alpacaErrorResponse(w, r, 0x40C, "Property not implemented by this driver.")
	default:
		// If the name is invalid, return an InvalidValue error.
		alpacaErrorResponse(w, r, 0x401, fmt.Sprintf("Invalid SensorName: '%s'", sensorName))
	}
}

// handleObsCondRefresh handles the Refresh method.
// This is a PUT-only method.
func handleObsCondRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != "PUT" {
		alpacaErrorResponse(w, r, 0x405, "Method "+r.Method+" not allowed for refresh.")
		return
	}
	// This device updates caches automatically in the background, so a manual refresh
	// is not necessary. We can just acknowledge the call.
	alpacaEmptyResponse(w, r)
}

// --- Alpaca Response Helpers ---

// managementValueResponse is a helper for the management API endpoints, which are not
// wrapped in the standard alpacaHandler and thus don't have transaction IDs.
func managementValueResponse(w http.ResponseWriter, r *http.Request, value interface{}) {
	response := struct {
		Value               interface{} `json:"Value"`
		ClientTransactionID uint32      `json:"ClientTransactionID"`
		ServerTransactionID uint32      `json:"ServerTransactionID"`
		ErrorNumber         int         `json:"ErrorNumber"`
		ErrorMessage        string      `json:"ErrorMessage"`
	}{
		Value:               value,
		ClientTransactionID: 0, // Not available in this context
		ServerTransactionID: 0, // Not stateful for this endpoint
		ErrorNumber:         0,
		ErrorMessage:        "",
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

type AlpacaResponse struct {
	ClientTransactionID uint32 `json:"ClientTransactionID"`
	ServerTransactionID uint32 `json:"ServerTransactionID"`
	ErrorNumber         int    `json:"ErrorNumber"`
	ErrorMessage        string `json:"ErrorMessage"`
}
type AlpacaValueResponse struct {
	AlpacaResponse
	Value interface{} `json:"Value"`
}

// getFormValueIgnoreCase retrieves the first value for a given key from the request form.
// It is case-insensitive for GET requests, but case-sensitive for PUT requests
// to satisfy the contradictory requirements of the ASCOM conformance checker.
func getFormValueIgnoreCase(r *http.Request, key string) (string, bool) {
	// The form should be parsed by the alpacaHandler middleware already.

	// The ASCOM conformance checker has contradictory requirements. For GETs, it
	// requires case-insensitivity as per the Alpaca spec. For PUTs, it issues
	// an error if the server accepts incorrectly-cased parameters. Therefore,
	// we must be case-sensitive for PUTs to pass the test.
	if r.Method == "PUT" {
		if values, ok := r.Form[key]; ok {
			if len(values) > 0 {
				return values[0], true
			}
			return "", true // Key exists, but has no value.
		}
		return "", false // Key not found with correct case.
	}

	// For all other methods (like GET), we follow the Alpaca spec and are case-insensitive.
	for k, values := range r.Form {
		if strings.EqualFold(k, key) {
			if len(values) > 0 {
				return values[0], true
			}
			return "", true // Key exists but has no value.
		}
	}
	return "", false // Key not found.
}

// parseSwitchID extracts the 'Id' parameter case-insensitively from the request,
// converts it to an integer, and validates it against the known switch IDs.
// It returns the integer ID and a boolean indicating success.
// If it returns false, it has already written an Alpaca error response.
func parseSwitchID(w http.ResponseWriter, r *http.Request) (int, bool) {
	idStr, ok := getFormValueIgnoreCase(r, "Id")
	if !ok || idStr == "" {
		alpacaErrorResponse(w, r, http.StatusBadRequest, "Invalid or missing switch ID")
		return 0, false
	}
	id, err := strconv.Atoi(idStr)
	if err != nil {
		// This handles cases where Id is not a valid integer.
		alpacaErrorResponse(w, r, http.StatusBadRequest, "Invalid or missing switch ID")
		return 0, false
	}
	if _, ok := switchIDMap[id]; !ok {
		alpacaErrorResponse(w, r, http.StatusBadRequest, "Invalid switch ID")
		return 0, false
	}
	return id, true
}

func alpacaHandler(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// For GET, params are in URL query. For PUT, they are in the body.
		logDebug("HTTP Request: %s %s", r.Method, r.URL.Path)
		// ParseForm handles both. It's safe to call multiple times.
		if err := r.ParseForm(); err != nil {
			logWarn("Error parsing form for request %s %s: %v", r.Method, r.URL.Path, err)
			// Continue, as some requests might not have a form to parse.
		}

		// Alpaca spec requires case-insensitive parameter names.
		// We must read ClientTransactionID and ClientID case-insensitively.
		if txIDStr, ok := getFormValueIgnoreCase(r, "ClientTransactionID"); ok {
			txID, _ := strconv.ParseUint(txIDStr, 10, 32) // Defaults to 0 on error
			atomic.StoreUint32(&clientTransactionID, uint32(txID))
		} else {
			// If the parameter is not present, the spec says the value is 0.
			// Storing 0 ensures we don't use a stale ID from a previous request.
			atomic.StoreUint32(&clientTransactionID, 0)
		}

		// We don't use ClientID in this proxy, but a conformance checker might
		// expect us to handle its presence. We just acknowledge it here.
		if _, ok := getFormValueIgnoreCase(r, "ClientID"); ok {
			// You could store or log this if needed, e.g.:
			// clientID, _ := strconv.ParseUint(idStr, 10, 32)
		}

		fn(w, r)
	}
}

func writeAlpacaResponse(w http.ResponseWriter, r *http.Request, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func alpacaEmptyResponse(w http.ResponseWriter, r *http.Request) {
	resp := AlpacaResponse{
		ClientTransactionID: atomic.LoadUint32(&clientTransactionID),
		ServerTransactionID: atomic.AddUint32(&serverTransactionID, 1),
	}
	writeAlpacaResponse(w, r, resp)
}

func alpacaStringListResponse(w http.ResponseWriter, r *http.Request, value []string) {
	resp := AlpacaValueResponse{
		AlpacaResponse: AlpacaResponse{
			ClientTransactionID: atomic.LoadUint32(&clientTransactionID),
			ServerTransactionID: atomic.AddUint32(&serverTransactionID, 1),
		},
		Value: value,
	}
	writeAlpacaResponse(w, r, resp)
}

func alpacaErrorResponse(w http.ResponseWriter, r *http.Request, errNum int, errMsg string) {
	httpStatus := http.StatusOK
	if errNum >= 400 && errNum < 600 {
		httpStatus = errNum
	}

	resp := AlpacaResponse{
		ClientTransactionID: atomic.LoadUint32(&clientTransactionID),
		ServerTransactionID: atomic.AddUint32(&serverTransactionID, 1),
		ErrorNumber:         errNum,
		ErrorMessage:        errMsg,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	logError("Alpaca request failed with error %d: %s", errNum, errMsg)
	json.NewEncoder(w).Encode(resp)
}

func alpacaStringResponse(w http.ResponseWriter, r *http.Request, value string) {
	resp := AlpacaValueResponse{
		AlpacaResponse: AlpacaResponse{
			ClientTransactionID: atomic.LoadUint32(&clientTransactionID),
			ServerTransactionID: atomic.AddUint32(&serverTransactionID, 1),
		},
		Value: value,
	}
	writeAlpacaResponse(w, r, resp)
}

func alpacaIntResponse(w http.ResponseWriter, r *http.Request, value int) {
	resp := AlpacaValueResponse{
		AlpacaResponse: AlpacaResponse{
			ClientTransactionID: atomic.LoadUint32(&clientTransactionID),
			ServerTransactionID: atomic.AddUint32(&serverTransactionID, 1),
		},
		Value: value,
	}
	writeAlpacaResponse(w, r, resp)
}

func alpacaFloatResponse(w http.ResponseWriter, r *http.Request, value float64) {
	resp := AlpacaValueResponse{
		AlpacaResponse: AlpacaResponse{
			ClientTransactionID: atomic.LoadUint32(&clientTransactionID),
			ServerTransactionID: atomic.AddUint32(&serverTransactionID, 1),
		},
		Value: value,
	}
	writeAlpacaResponse(w, r, resp)
}

func alpacaBoolResponse(w http.ResponseWriter, r *http.Request, value bool) {
	resp := AlpacaValueResponse{
		AlpacaResponse: AlpacaResponse{
			ClientTransactionID: atomic.LoadUint32(&clientTransactionID),
			ServerTransactionID: atomic.AddUint32(&serverTransactionID, 1),
		},
		Value: value,
	}
	writeAlpacaResponse(w, r, resp)
}
