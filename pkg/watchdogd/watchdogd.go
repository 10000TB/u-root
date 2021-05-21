// Copyright 2021 the u-root Authors. All rights reserved
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// watchdogd implements a background process which periodically issues a
// keepalive.
//
// It starts in the running+armed state:
//
//              | watchdogd Running     | watchdogd Stopped
//     ---------+-----------------------+--------------------------
//     Watchdog | watchdogd is actively | machine will soon reboot
//     Armed    | keeping machine alive |
//     ---------+-----------------------+--------------------------
//     Watchdog | a hang will not       | a hang will not reboot
//     Disarmed | reboot the machine    | the machine
//

package watchdogd

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/u-root/u-root/pkg/watchdog"
	"golang.org/x/sys/unix"
)

// UDS is the name of daemon's unix domain socket.
const UDS = "/tmp/watchdogd"

const (
	OpStop     = iota // 0 - Stop the watchdogd petting.
	OpContinue        // 1 - Continue the watchdogd petting.
	OpDisarm          // 2 - Disarm the watchdog.
	OpArm             // 3 - Arm the watchdog.
)

const (
	OpResultOK    = iota // 0 - OK
	OpResultERROR        // 1 - ERROR
)

const (
	opStopPettingTimeoutSeconds = 10
)

// currentOpts is current operating parameters for the daemon.
//
// It is assigned at the first call of Run and updated on each subsequent call of it.
var currentOpts *DaemonOpts

// currentWd is an open file descriptor to the watchdog device specified in the daemon options.
var currentWd *watchdog.Watchdog

// pettingOp syncs the signal to continue or stop petting the watchdog.
var pettingOp chan int = make(chan int)

// pettingOn indicate if there is an active petting session.
var pettingOn bool = false

// DaemonOpts contain options for the watchdog daemon.
type DaemonOpts struct {
	// Dev is the watchdog device. Ex: /dev/watchdog
	Dev string

	// nil uses the preset values. 0 disables the timeout.
	Timeout, PreTimeout *time.Duration

	// KeepAlive is the length of the keep alive interval.
	KeepAlive time.Duration

	// Monitors are called before each keepalive interval. If any monitor
	// function returns an error, the .
	Monitors []func() error
}

// MonitorOops return an error if the kernel logs contain an oops.
func MonitorOops() error {
	dmesg := make([]byte, 256*1024)
	n, err := unix.Klogctl(unix.SYSLOG_ACTION_READ_ALL, dmesg)
	if err != nil {
		return fmt.Errorf("syslog failed: %v", err)
	}
	if strings.Contains(string(dmesg[:n]), "Oops:") {
		return fmt.Errorf("founds Oops in dmesg")
	}
	return nil
}

// startServing enters a loop of accepting and processing next incoming watchdogd operation call.
func startServing(l *net.UnixListener) {
	for { // All requests are processed sequentially.
		c, err := l.AcceptUnix()
		if err != nil {
			log.Printf("watchdogd: %v", err)
			continue
		}
		b := make([]byte, 1) // Expect single byte operation instruction.
		nr, err := c.Read(b[:])
		if err != nil || nr != 1 {
			log.Printf("watchdogd: Failed to read oeration bit, err: %v, read: %d", err, nr)
		}
		op := int(b[0])
		r := OpResultERROR
		switch op {
		case OpStop:
			r = stopPetting()
		case OpContinue:
			r = startPetting()
		case OpArm:
			r = armWatchdog()
		case OpDisarm:
			r = disarmWatchdog()
		}
		c.Write([]byte{byte(r)})
		c.Close()
	}
}

// setupListener sets up a new "unix" network listener for the daemon.
func setupListener() (*net.UnixListener, func(), error) {
	os.Remove(UDS)

	l, err := net.ListenUnix("unix", &net.UnixAddr{UDS, "unix"})
	if err != nil {
		return nil, nil, fmt.Errorf("watchdogd: %v", err)
	}
	cleanup := func() {
		os.Remove(UDS)
	}
	return l, cleanup, nil
}

// armWatchdog starts watchdog timer.
func armWatchdog() int {
	if currentOpts == nil {
		log.Printf("Current daemon opts is nil, don't know how to arm Watchdog")
		return OpResultERROR
	}
	wd, err := watchdog.Open(currentOpts.Dev)
	if err != nil {
		// Most likely cause is /dev/watchdog does not exist.
		// Second most likely cause is another process (perhaps
		// another watchdogd?) has the file open.
		log.Printf("Failed to arm: %v", err)
		return OpResultERROR
	}
	if currentOpts.Timeout != nil {
		if err := wd.SetTimeout(*currentOpts.Timeout); err != nil {
			currentWd.Close()
			log.Printf("Failed to set timeout: %v", err)
			return OpResultERROR
		}
	}
	if currentOpts.PreTimeout != nil {
		if err := wd.SetPreTimeout(*currentOpts.PreTimeout); err != nil {
			currentWd.Close()
			log.Printf("Failed to set pretimeout: %v", err)
			return OpResultERROR
		}
	}
	currentWd = wd
	log.Printf("watchdogd: Watchdog armed")
	return OpResultOK
}

// disarmWatchdog disarm the watchdog if already armed.
func disarmWatchdog() int {
	if currentWd == nil {
		log.Printf("watchdogd: No armed Watchdog")
		return OpResultOK
	}
	if err := currentWd.MagicClose(); err != nil {
		log.Printf("watchdogd: Failed to disarm watchdog: %v", err)
		return OpResultERROR
	}
	return OpResultOK
}

// doPetting sends keepalive signal to Watchdog when necessary.
//
// If at least one of the custom monitors failed check(s), it won't send a keepalive
// signal.
func doPetting() error {
	if currentWd == nil {
		return fmt.Errorf("no reference to any Watchdog")
	}
	if err := doMonitors(currentOpts.Monitors); err != nil {
		return fmt.Errorf("won't keepalive since at least one of the custom monitors failed: %v", err)
	}
	if err := currentWd.KeepAlive(); err != nil {
		return err
	}
	return nil
}

// startPetting starts Watchdog petting in a new goroutine.
func startPetting() int {
	if pettingOn {
		log.Printf("watchdogd: Petting ongoing")
		return OpResultERROR
	}

	go func() {
		pettingOn = true
		defer func() { pettingOn = false }()
		for {
			select {
			case op := <-pettingOp:
				if op == OpStop {
					return
				}
			case <-time.After(currentOpts.KeepAlive):
				if err := doPetting(); err != nil {
					log.Printf("watchdogd: Failed to keeplive: %v", err)
					// Keep trying to pet until the watchdog times out.
				}
			}
		}
	}()
	return OpResultOK
}

// stopPetting stops an ongoing petting process if there is.
func stopPetting() int {
	if !pettingOn {
		return OpResultOK
	} // No petting on, simply return.
	r := OpResultOK
	erredOut := func() {
		<-pettingOp
		log.Printf("Stop petting times out after %d seconds", opStopPettingTimeoutSeconds)
		r = OpResultERROR
	}
	// It will time out when there is no active petting.
	t := time.AfterFunc(opStopPettingTimeoutSeconds*time.Second, erredOut)
	defer t.Stop()
	pettingOp <- OpStop
	return r
}

// Run starts up the daemon.
//
// That includes:
// 1) Starts listening for watchdog(d) operation requests over unix network.
// 2) Arm the watchdog timer if it is not already armed.
// 3) Starts petting the watchdog timer.
func Run(ctx context.Context, opts *DaemonOpts) error {
	defer log.Printf("watchdogd: Daemon quit")
	currentOpts = opts

	l, cleanup, err := setupListener()
	if err != nil {
		return fmt.Errorf("watchdogd: Failed to setup server: %v", err)
	}
	go func() {
		startServing(l)
	}()

	if r := armWatchdog(); r != OpResultOK {
		return fmt.Errorf("watchdogd: Initial arm failed")
	}

	for {
		select {
		case <-ctx.Done():
			cleanup()
		}
	}

}

// doMonitors is a helper function to run the monitors.
//
// If there is anything wrong identified, it serves as a signal to stop
// petting Watchdog.
func doMonitors(monitors []func() error) error {
	for _, m := range monitors {
		if err := m(); err != nil {
			return err
		}
	}
	// All monitors return normal.
	return nil
}

type client struct {
	conn *net.UnixConn
}

func (c *client) Stop() error {
	return sendAndCheckResult(c.conn, OpStop)
}

func (c *client) Continue() error {
	return sendAndCheckResult(c.conn, OpContinue)
}

func (c *client) Disarm() error {
	return sendAndCheckResult(c.conn, OpDisarm)
}

func (c *client) Arm() error {
	return sendAndCheckResult(c.conn, OpArm)
}

// sendAndCheckResult sends operation bit and evaluates result.
func sendAndCheckResult(c *net.UnixConn, op int) error {
	n, err := c.Write([]byte{byte(op)})
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("no error; but message not delivered neither")
	}
	buf := make([]byte, 1)
	n, err = c.Read(buf[:])
	if err != nil {
		return fmt.Errorf("error reading from server: %v", err)
	}
	if n != 1 {
		return fmt.Errorf("received message from server, but incorrect length")
	}
	r := int(buf[0])
	if r != OpResultOK {
		return fmt.Errorf("non-zero op result code: %d", r)
	}
	return nil
}

func NewClient() (*client, error) {
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{UDS, "unix"})
	if err != nil {
		return nil, err
	}
	return &client{conn: conn}, nil
}
