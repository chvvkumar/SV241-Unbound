import os
Import("env")

go_bin_path = os.path.join(os.path.expanduser("~"), "go", "bin")
goversioninfo_path = os.path.join(go_bin_path, "goversioninfo")

env.AddCustomTarget(
    "buildgoproxyexe",
    None,
    [
        "go install github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest",
        f'"{goversioninfo_path}" -o AscomAlpacaProxy/resource.syso AscomAlpacaProxy/versioninfo.json',
        "cd AscomAlpacaProxy && go build -ldflags \"-H=windowsgui\" -o build/AscomAlpacaProxy.exe .",
        "del AscomAlpacaProxy\\resource.syso"
    ],
    "Build Go Proxy Windows Executable",
    "Builds the Go proxy application for Windows"
)
