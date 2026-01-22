# Development / Test Build

This folder contains a **development version** (`AscomAlpacaProxy.exe`) of the proxy driver for testing purposes. It may contain new features or bug fixes that have not yet been released officially.

## How to Install

1.  **Stop the Proxy:** Ensure that the current "SV241 Ascom Alpaca Proxy" is closed (check the system tray or Task Manager).
2.  **Copy the File:** Copy the `AscomAlpacaProxy.exe` from this folder.
3.  **Overwrite:** Paste it into your installation directory, typically:
    *   `C:\Program Files (x86)\SV241 Ascom Alpaca Proxy`
    *   (Replace the existing file when prompted)
4.  **Restart:** Start the proxy as usual.

## How to Revert (Rollback)

To go back to the stable official version:

1.  Simply run the **original Setup Installer** (Setup.exe) again.
2.  It will overwrite this development version with the clean, stable release.
