package main

import (
	"bonan.se/ipmon"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
)

var (
	errLog  = log.New(os.Stderr, "[ERROR] ", 0)
	infoLog = log.New(os.Stderr, "[INFO] ", 0)
	dbgLog  = log.New(io.Discard, "[DEBUG] ", 0)
	sdConn  *net.UnixConn
)

func main() {
	flgDebug := flag.Bool("d", false, "Enable debug logging")
	flgJson := flag.Bool("j", false, "Send JSON to process stdin")
	flgInterval := flag.Int("i", 0, "Trigger periodic updates (seconds)")

	flag.Parse()
	Status("Starting")

	if os.Getenv("DEBUG") == "1" || *flgDebug {
		dbgLog.SetOutput(os.Stderr)
		ipmon.Debug.SetOutput(os.Stderr)
	}

	argv := flag.Args()

	cmdName := ""
	var args []string

	if len(argv) > 0 {
		cmdName = argv[0]
		args = argv[1:]
	}

	rdy := false

	ctx := context.Background()
	if err := ipmon.Monitor(ctx, *flgInterval, func(upd *ipmon.Update) {

		if !rdy {
			Status("Running")
			Ready()
			rdy = true
		}

		infoLog.Printf("Update: %s %v %+v Link[%s] GW[%s] Source[%s]", upd.Type, upd.Change, upd.Address, upd.Link, upd.Gateway, upd.Source)

		if cmdName != "" {
			newEnv := upd.MarshalEnv()
			cmd := exec.CommandContext(ctx, cmdName, args...)

			pr, pw := io.Pipe()

			cmd.Stderr = os.Stderr
			cmd.Stdin = pr
			cmd.Stdout = os.Stdout
			cmd.Env = []string{}
			for _, v := range os.Environ() {
				if strings.HasPrefix(v, "NOTIFY_SOCKET=") {
					continue
				}
				cmd.Env = append(cmd.Env, v)
			}
			cmd.Env = append(cmd.Env, newEnv...)
			if err := cmd.Start(); err != nil {
				errLog.Print(err)
			}

			if *flgJson {
				je := json.NewEncoder(pw)
				if err := je.Encode(upd); err != nil {
					errLog.Printf("Unable to encode JSON: %v", err)
				}
			}
			_ = pw.Close()
			if err := cmd.Wait(); err != nil {
				errLog.Print(err)
			}
		}

	}); err != nil {
		errLog.Printf("Error while monitoring: %v", err)
	}
}

func notifyOpen() bool {
	if sdConn != nil {
		return true
	}
	sockName := os.Getenv("NOTIFY_SOCKET")
	if sockName == "" {
		return false
	}
	if conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{
		Name: sockName,
		Net:  "unixgram",
	}); err != nil {
		return false
	} else {
		sdConn = conn
		return true
	}
}

func sdnotify(state string) {
	if notifyOpen() {
		data := []byte(state + "\n")
		if _, err := sdConn.Write(data); err != nil {
			_ = sdConn.Close()
			sdConn = nil
			if notifyOpen() {
				_, _ = sdConn.Write(data)
			}
		}
	}
}

func Ready()            { sdnotify("READY=1") }
func Reloading()        { sdnotify("RELOADING=1") }
func Stopping()         { sdnotify("STOPPING=1") }
func Watchdog()         { sdnotify("WATCHDOG=1") }
func Status(str string) { sdnotify(fmt.Sprintf("STATUS=%s", str)) }
