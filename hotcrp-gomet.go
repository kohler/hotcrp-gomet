package main

import (
	"flag"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"strconv"
)

var directoryWatcher func(string)

func nfilesGet() int {
	var rlimit unix.Rlimit
	_ = unix.Getrlimit(unix.RLIMIT_NOFILE, &rlimit)
	return int(rlimit.Cur)
}

func nfilesSet(n int) int {
	var rlimit unix.Rlimit
	_ = unix.Getrlimit(unix.RLIMIT_NOFILE, &rlimit)
	if n <= 0 {
		rlimit.Cur = rlimit.Max
	} else if uint64(n) < rlimit.Max {
		rlimit.Cur = uint64(n)
	}
	_ = unix.Setrlimit(unix.RLIMIT_NOFILE, &rlimit)
	return nfilesGet()
}

func userSet(username string) {
	u, err := user.Lookup(username)
	if err != nil {
		log.Fatal(err)
	}
	gid, _ := strconv.Atoi(u.Gid)
	err = unix.Setgid(gid)
	if err != nil {
		log.Fatal(err)
	}
	uid, _ := strconv.Atoi(u.Uid)
	err = unix.Setuid(uid)
	if err != nil {
		log.Fatal(err)
	}
}

func main() {
	var fg, bgChild bool
	flag.BoolVar(&fg, "fg", false, "run in foreground")
	flag.BoolVar(&bgChild, "bg-child", false, "")

	var nfiles int = nfilesGet()
	var nfilesReq int
	flag.IntVar(&nfilesReq, "nfiles", nfiles, "maximum number of files")
	flag.IntVar(&nfilesReq, "n", nfiles, "maximum number of files")

	var port int = 20444
	flag.IntVar(&port, "port", port, "listening port")
	flag.IntVar(&port, "p", port, "listening port")

	var user string
	flag.StringVar(&user, "user", "", "run as user")
	flag.StringVar(&user, "u", "", "run as user")

	var watchDirectory string
	flag.StringVar(&watchDirectory, "watch-directory", "", "directory to watch for updates")
	flag.StringVar(&watchDirectory, "d", "", "directory to watch for updates")
	flag.StringVar(&watchDirectory, "update-directory", "", "")

	var pidFile string
	flag.StringVar(&pidFile, "pid-file", "", "file to write process PID")

	var logFile string
	flag.StringVar(&logFile, "log", "", "log file")
	flag.StringVar(&logFile, "log-file", "", "log file")

	flag.Parse()

	var logw io.Writer = ioutil.Discard
	if logFile != "" {
		logw, err := os.OpenFile(logFile, os.O_WRONLY | os.O_APPEND | os.O_CREATE, 0660)
		if err != nil {
			log.Fatal(err)
		}
		log.SetOutput(io.MultiWriter(logw, os.Stderr))
	}

	if nfiles != nfilesReq {
		nfiles = nfilesSet(nfilesReq)
		if nfiles < nfilesReq {
			log.Printf("limited to %d open files\n", nfiles)
		}
	}

	var listener net.Listener
	var err error
	if bgChild {
		listener, err = net.FileListener(os.NewFile(3, "listen-fd"))
	} else {
		listener, err = net.Listen("tcp", fmt.Sprintf(":%d", port))
	}
	if err != nil {
		log.Fatal(err)
	}

	if user != "" {
		userSet(user)
	}

	if watchDirectory != "" && directoryWatcher == nil {
		log.Fatalf("this platform does not support `--update-directory`")
	} else if watchDirectory != "" {
		go directoryWatcher(watchDirectory)
	}

	if pidFile != "" {
		err := ioutil.WriteFile(pidFile, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0660)
		if err != nil {
			log.Fatal(err)
		}
	}

	if !fg && !bgChild {
		tcpListenFd, err := listener.(*net.TCPListener).File()
		if err != nil {
			log.Fatal(err)
		}
		args := []string{"--bg-child"}
		if logFile != "" {
			args = append(args, "--log-file", logFile)
		}
		if pidFile != "" {
			args = append(args, "--pid-file", pidFile)
		}
		if user != "" {
			args = append(args, "--user", user)
		}
		if watchDirectory != "" {
			args = append(args, "--watch-directory", watchDirectory)
		}
		cmd := exec.Command(os.Args[0], args...)
		cmd.ExtraFiles = []*os.File{tcpListenFd}
		if err = cmd.Start(); err != nil {
			log.Fatal(err)
		}
		os.Exit(0)
	}

	if bgChild {
		unix.Setpgid(0, 0)
		signal.Ignore(unix.SIGHUP)
		log.SetOutput(logw)
		os.Stdin.Close()
		os.Stdout.Close()
		os.Stderr.Close()
	}

	http.HandleFunc("/poll", PollRequest)
	http.HandleFunc("/update", UpdateRequest)
	log.Fatal(http.Serve(listener, nil))
}
