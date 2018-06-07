// Continually run a batch of environments and take
// guidance from a remote agent.

package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"math/rand"
	"net/http"
	"os/exec"
	"strconv"
	"time"

	"github.com/unixpickle/essentials"
	"github.com/unixpickle/muniverse/chrome"
)

type Args struct {
	RedisHost     string
	ChannelPrefix string
	NumEnvs       int
	Chrome        string

	ImageSize int

	ServerAddr string
	ServerPath string
}

func main() {
	rand.Seed(time.Now().UnixNano())

	var args Args

	flag.StringVar(&args.RedisHost, "redis", "qwop-redis:6379", "the Redis host")
	flag.StringVar(&args.ChannelPrefix, "channel", "qwop-worker", "the prefix for channel names")
	flag.IntVar(&args.NumEnvs, "envs", 4, "number of environments to run")
	flag.StringVar(&args.Chrome, "chrome", "chromium-browser", "Chrome executable name")

	flag.IntVar(&args.ImageSize, "image-size", 84, "size of observation images")

	flag.StringVar(&args.ServerAddr, "server-addr", "127.0.0.1:8080", "address for the server")
	flag.StringVar(&args.ServerPath, "server-path", "/web", "URL for the server")

	flag.Parse()

	go GameDataServer(&args)

	// Wait for server to startup.
	time.Sleep(time.Second)

	for i := 0; i < args.NumEnvs; i++ {
		go RunEnvironment(&args, i)
	}

	select {}
}

// GameDataServer creates a webserver for the game.
func GameDataServer(args *Args) {
	http.Handle("/", http.FileServer(http.Dir(args.ServerPath)))
	http.ListenAndServe(args.ServerAddr, nil)
}

// RunEnvironment starts an environment and serves it to
// the master agent.
func RunEnvironment(args *Args, idx int) {
	session, err := NewSession(args.RedisHost, args.ChannelPrefix)
	essentials.Must(err)
	defer session.Close()

	chromeClient, chromeProc, err := StartChrome(args.Chrome, args.ServerAddr, 9222+idx)
	essentials.Must(err)
	defer func() {
		chromeClient.Close()
		chromeProc.Process.Kill()
		go chromeProc.Wait()
	}()

	log.Printf("%s: waiting for environment", session.EnvID())
	essentials.Must(WaitForEnv(chromeClient))
	newEpisode := true
	for {
		log.Printf("%s: encoding state", session.EnvID())
		state, err := StateForEnv(chromeClient, newEpisode, args.ImageSize)
		essentials.Must(err)
		log.Printf("%s: sending state", session.EnvID())
		essentials.Must(session.SendState(state))
		log.Printf("%s: receiving action", session.EnvID())
		action, err := session.ReceiveAct()
		log.Printf("%s: stepping environment with action %v", session.EnvID(), action[:])
		essentials.Must(err)
		done, err := StepEnv(chromeClient, action)
		essentials.Must(err)
		if done {
			essentials.Must(ResetEnv(chromeClient))
			newEpisode = true
		} else {
			newEpisode = false
		}
	}
}

// Start headless chrome in a background process.
func StartChrome(chromeExec, serverAddr string, port int) (*chrome.Conn, *exec.Cmd, error) {
	command := exec.Command(chromeExec,
		"--no-sandbox",
		"--mute-audio",
		"--headless",
		"--remote-debugging-port="+strconv.Itoa(port),
		"--remote-debugging-address=0.0.0.0",
		"--window-size=640x400",
		"http://"+serverAddr+"/index.html")
	if err := command.Start(); err != nil {
		return nil, nil, err
	}
	for i := 0; i < 20; i++ {
		endpoints, err := chrome.Endpoints(context.Background(), "localhost:"+strconv.Itoa(port))
		if err == nil {
			for _, ep := range endpoints {
				if ep.Type == "page" && ep.WebSocketURL != "" {
					conn, err := chrome.NewConn(context.Background(), ep.WebSocketURL)
					if err != nil {
						command.Process.Kill()
						go command.Wait()
						return nil, nil, essentials.AddCtx("start chrome", err)
					}
					return conn, command, nil
				}
			}
		}
		time.Sleep(time.Second)
	}
	command.Process.Kill()
	go command.Wait()
	return nil, nil, errors.New("start chrome: could not list endpoints")
}
