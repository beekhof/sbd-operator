# Watchdog Package

The `watchdog` package provides a Go interface for interacting with Linux kernel watchdog devices. It allows applications to control hardware watchdog timers to ensure system reliability through automatic reboot capabilities.

## Overview

Hardware watchdog devices are used to automatically reset the system if the software becomes unresponsive. The application must periodically "pet" or "kick" the watchdog to prevent an automatic system reset.

## Features

- **Device Management**: Opens and manages watchdog device files (`/dev/watchdog`, `/dev/watchdog0`, etc.)
- **Timer Reset**: Provides `Pet()` method to reset the watchdog timer using Linux ioctl calls
- **Safe Closure**: Implements "magic close" functionality to safely disable watchdog on exit
- **Error Handling**: Comprehensive error handling for device operations
- **Thread Safety**: Safe for concurrent access (though typically used by single monitoring thread)

## Usage

### Basic Example

```go
package main

import (
    "log"
    "time"
    
    "github.com/medik8s/sbd-operator/pkg/watchdog"
)

func main() {
    // Open the watchdog device
    wd, err := watchdog.New("/dev/watchdog")
    if err != nil {
        log.Fatalf("Failed to open watchdog: %v", err)
    }
    defer wd.Close()
    
    // Pet the watchdog every 10 seconds
    ticker := time.NewTicker(10 * time.Second)
    defer ticker.Stop()
    
    for {
        select {
        case <-ticker.C:
            if err := wd.Pet(); err != nil {
                log.Printf("Failed to pet watchdog: %v", err)
            }
        }
    }
}
```

### API Reference

#### `New(path string) (*Watchdog, error)`

Creates a new Watchdog instance by opening the device at the specified path.

- **Parameters**: `path` - filesystem path to watchdog device (e.g., "/dev/watchdog")
- **Returns**: Watchdog instance or error
- **Note**: Opening a watchdog device typically activates the timer

#### `(*Watchdog) Pet() error`

Resets the watchdog timer to prevent system reset.

- **Returns**: Error if the operation fails
- **Note**: Must be called before the timeout period expires

#### `(*Watchdog) Close() error`

Closes the watchdog device and attempts to disable the timer.

- **Returns**: Error if the close operation fails
- **Note**: Uses "magic close" (writing 'V') to safely disable some watchdog devices

#### `(*Watchdog) IsOpen() bool`

Returns true if the watchdog device is currently open.

#### `(*Watchdog) Path() string`

Returns the filesystem path of the watchdog device.

## Important Notes

### Device Behavior

- **Activation**: Opening a watchdog device usually activates the timer immediately
- **Timeout**: Each watchdog has a specific timeout period (typically 15-60 seconds)
- **Reset Frequency**: Pet the watchdog at least twice as often as the timeout period
- **System Reset**: Failure to pet within the timeout causes an immediate system reset

### Driver Variations

Different watchdog drivers may have varying behaviors:

- **Magic Close**: Some require writing 'V' before closing to disable the timer
- **Automatic Disable**: Others automatically disable when the device is closed
- **Persistent**: Some continue running even after the application exits

### Error Handling

- **Device Not Found**: `/dev/watchdog` may not exist on systems without hardware watchdog
- **Permission Denied**: Watchdog devices typically require root privileges
- **IOCTL Failures**: Regular files will fail ioctl operations (expected in tests)

### Testing

The package includes comprehensive unit tests that use temporary files to simulate watchdog devices. The ioctl operations will fail on regular files, which is expected and handled gracefully.

```bash
# Run tests
go test ./pkg/watchdog

# Run demo
go run ./cmd/watchdog-demo/main.go
```

## Linux Watchdog IOCTL Commands

The package uses standard Linux watchdog ioctl commands:

- `WDIOC_KEEPALIVE` (0x40045705): Reset the watchdog timer
- `WDIOC_SETTIMEOUT` (0x40045706): Set timeout period  
- `WDIOC_GETTIMEOUT` (0x40045707): Get current timeout

## Security Considerations

- Watchdog operations typically require root privileges
- Improper use can cause unexpected system resets
- Always implement proper error handling and logging
- Consider graceful shutdown procedures

## Dependencies

- `golang.org/x/sys/unix`: For Linux system calls and ioctl operations
- Standard Go library packages (`os`, `fmt`, etc.)

## License

Copyright 2025 - Licensed under the Apache License, Version 2.0 