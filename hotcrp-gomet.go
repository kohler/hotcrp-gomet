package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"golang.org/x/sys/unix"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os/signal"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"time"
)

//const siteStatusTimeout = 120 * time.Second
const siteStatusTimeout = 10 * time.Second
const siteErrorStatusTimeout = 5 * time.Second

var directoryWatcher func(string)


type TrackerSequencer float64

func (seq TrackerSequencer) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf("%f", seq)), nil
}


type SiteStatus struct {
	Status string
	Sequencer TrackerSequencer
	Error error
}

type Site struct {
	siteurl string
	createAt time.Time
	accessAt time.Time
	status SiteStatus
	statusSetAt time.Time
	closed bool

	mu sync.Mutex
	statusRefreshing bool
	statusWaiters []chan<- SiteStatus
	statusInterest int
	statusLooper chan<- struct{}
}

func NewSite(siteurl string) *Site {
	site := new(Site)
	site.siteurl = siteurl
	site.createAt = time.Now()
	site.statusWaiters = make([]chan<- SiteStatus, 0, 8)
	ch := make(chan struct{}, 2)
	site.statusLooper = ch
	go site.statusLoop(ch)
	return site
}

func (site *Site) statusExpiry() time.Time {
	timeout := time.Duration(siteStatusTimeout)
	if site.status.Status == "" {
		timeout = time.Duration(siteErrorStatusTimeout)
	}
	return site.statusSetAt.Add(timeout)
}

func (site *Site) statusExpired() bool {
	return time.Now().After(site.statusExpiry())
}

func (site *Site) statusLoop(ch <-chan struct{}) {
	for !site.closed {
		site.mu.Lock()
		expiry := site.statusExpiry().Add(-2 * time.Second)
		now := time.Now()
		var timech <-chan time.Time
		if site.statusInterest != 0 && now.Before(expiry) {
			timech = time.After(expiry.Sub(now))
		}
		site.mu.Unlock()

		select {
		case <-timech:
		case <-ch:
		}
	}
}


type TrackerStatusResponse struct {
	Ok bool `json:"ok"`
	Error string `json:"error"`
	TrackerStatus string `json:"tracker_status"`
	Sequencer TrackerSequencer `json:"tracker_status_at"`
}

func (site *Site) renewStatus() {
	site.mu.Lock()
	if !site.statusRefreshing {
		site.statusRefreshing = true

		site.mu.Unlock()
		resp, err := http.Get(site.siteurl + "api.php?fn=trackerstatus")
		var respbody []byte
		if err == nil {
			respbody, err = ioutil.ReadAll(resp.Body)
		}
		fmt.Printf("%sapi/trackerstatus -> %s\n", site.siteurl, string(respbody))
		statusResponse := TrackerStatusResponse{}
		if err == nil {
			err = json.Unmarshal(respbody, &statusResponse)
		}
		site.mu.Lock()

		newStatus := SiteStatus{"", 0, err}
		if err == nil && statusResponse.Ok {
			newStatus.Status = statusResponse.TrackerStatus
			newStatus.Sequencer = statusResponse.Sequencer
		} else if err == nil {
			newStatus.Error = fmt.Errorf("%s", statusResponse.Error)
		}
		site.Update(newStatus)
		site.statusRefreshing = false
	}
	site.mu.Unlock()
}

func (site *Site) Update(newStatus SiteStatus) {
	if newStatus.Error != nil || site.status.Status == "" || site.status.Sequencer < newStatus.Sequencer {
		site.status = newStatus
	}
	site.statusSetAt = time.Now()
	for _, ch := range site.statusWaiters {
		ch <- site.status
	}
	site.statusWaiters = site.statusWaiters[0:0]
	site.statusLooper <- struct{}{}
}

func (site *Site) Status() <-chan SiteStatus {
	ch := make(chan SiteStatus, 1)
	site.mu.Lock()
	if site.statusExpired() {
		site.statusWaiters = append(site.statusWaiters, ch)
		go site.renewStatus()
	} else {
		ch <- site.status
	}
	site.mu.Unlock()
	return ch
}

func (site *Site) DifferentStatus(status string) <-chan SiteStatus {
	ch := make(chan SiteStatus, 1)
	site.mu.Lock()
	if site.statusExpired() || site.status.Status == status {
		site.statusInterest++
		if site.statusInterest == 1 {
			site.statusLooper <- struct{}{}
		}
		go func() {
			lch := make(chan SiteStatus, 1)
			site.mu.Lock()
			for site.statusExpired() || site.status.Status == status {
				site.statusWaiters = append(site.statusWaiters, lch)
				site.mu.Unlock()
				<-lch
				site.mu.Lock()
			}
			site.statusInterest--
			ch <- site.status
			site.mu.Unlock()
		}()
	} else {
		ch <- site.status
	}
	site.mu.Unlock()
	return ch
}


var (
	sitemapmu sync.Mutex
	sitemap map[string]*Site = make(map[string]*Site)
)

func LookupSite(s, host string, create bool) (*Site, error) {
	if s == "" {
		return nil, fmt.Errorf("missing conference")
	}

	u, err := url.Parse(s)
	if err != nil || !u.IsAbs() || (u.Host == "" && host == "") ||
		(u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("bad conference %q", s)
	}

	if u.Host == "" {
		u.Host = host
	}
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	siteurl := u.String()

	sitemapmu.Lock()
	site := sitemap[siteurl]
	if site == nil && create {
		site = NewSite(siteurl)
		sitemap[siteurl] = site
	}
	if site != nil {
		site.accessAt = time.Now()
	}
	sitemapmu.Unlock()

	return site, nil
}


type SiteResponse struct {
	Ok bool `json:"ok"`
	Status string `json:"tracker_status,omitempty"`
	Sequencer TrackerSequencer `json:"tracker_status_at,omitempty"`
	Error string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
}

func SiteRequest(w http.ResponseWriter, req *http.Request) {
	w.Header().Add("Access-Control-Allow-Origin", "*")
	w.Header().Add("Access-Control-Allow-Credentials", "true")
	w.Header().Add("Access-Control-Allow-Headers", "Accept-Encoding")
	w.Header().Add("Expires", "Mon, 26 Jul 1997 05:00:00 GMT")
	var result SiteResponse
	var status SiteStatus
	site, err := LookupSite(req.FormValue("conference"), req.Host, true)
	if err != nil {
		status = SiteStatus{Error: err}
	} else if poll := req.FormValue("poll"); poll != "" {
		status = <-site.DifferentStatus(poll)
	} else {
		status = <-site.Status()
	}
	if status.Error == nil {
		result.Ok = true
		result.Status = status.Status
		result.Sequencer = status.Sequencer
	} else {
		result.Error = status.Error.Error()
	}
	data, _ := json.Marshal(result)
	w.Write(data)
}


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
	var fg bool
	flag.BoolVar(&fg, "fg", false, "run in foreground")

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
	flag.StringVar(&watchDirectory, "update-directory", "", "directory to watch for updates")
	flag.StringVar(&watchDirectory, "d", "", "directory to watch for updates")

	flag.Parse()

	if nfiles != nfilesReq {
		nfiles = nfilesSet(nfilesReq)
		if nfiles < nfilesReq {
			log.Printf("limited to %d open files\n", nfiles)
		}
	}

	if user != "" {
		userSet(user)
	}

	if watchDirectory != "" && directoryWatcher == nil {
		log.Fatalf("this platform does not support `--update-directory`")
	} else if watchDirectory != "" {
		go directoryWatcher(watchDirectory)
	}

	if !fg {
		unix.Setpgid(0, 0)
		signal.Ignore(unix.SIGHUP)
		// XXX close stdin/stdout/stderr
	}

	http.HandleFunc("/", SiteRequest)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", port), nil))
}
