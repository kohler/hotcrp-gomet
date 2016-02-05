package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

//const siteStatusTimeout = 120 * time.Second
const siteStatusTimeout = 10 * time.Second
const siteErrorStatusTimeout = 5 * time.Second

type Site struct {
	siteurl string
	createAt time.Time
	accessAt time.Time
	status string
	statusError error
	statusSequencer float64
	statusSetAt time.Time

	mu sync.Mutex
	statusRefreshing bool
	statusWaiters []chan<- string
	statusInterest int
}

func (site *Site) statusExpiry() time.Time {
	timeout := time.Duration(siteStatusTimeout)
	if site.status == "" {
		timeout = time.Duration(siteErrorStatusTimeout)
	}
	return site.statusSetAt.Add(timeout)
}

func (site *Site) statusExpired() bool {
	return time.Now().After(site.statusExpiry())
}

type TrackerStatusResponse struct {
	Ok bool `json:"ok"`
	Error string `json:"error"`
	TrackerStatus string `json:"tracker_status"`
	Sequencer float64 `json:"tracker_status_at"`
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

		site.status = ""
		site.statusError = err
		site.statusSetAt = time.Now()
		if err != nil {
			// already have tracked error
		} else if !statusResponse.Ok {
			site.statusError = fmt.Errorf("%s", statusResponse.Error)
		} else if site.status != "" && statusResponse.Sequencer < site.statusSequencer {
			// ignore out-of-sequence response
		} else {
			site.status = statusResponse.TrackerStatus
			site.statusSequencer = statusResponse.Sequencer
		}

		for _, ch := range site.statusWaiters {
			ch <- site.status
		}
		site.statusWaiters = site.statusWaiters[0:0]

		site.statusRefreshing = false
	}
	site.mu.Unlock()
}

func (site *Site) statusLoop() {
	for {
		site.mu.Lock()
		if site.statusInterest == 0 {
			site.mu.Unlock()
			return
		}

		expiry := site.statusExpiry().Add(-2 * time.Second)
		now := time.Now()
		if now.Before(expiry) {
			site.mu.Unlock()
			<-time.After(expiry.Sub(now))
		} else {
			ch := make(chan string)
			site.statusWaiters = append(site.statusWaiters, ch)
			site.mu.Unlock()
			go site.renewStatus()
			<-ch
		}
	}
}

func (site *Site) Status() <-chan string {
	ch := make(chan string, 1)
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

func (site *Site) DifferentStatus(status string) <-chan string {
	ch := make(chan string, 1)
	site.mu.Lock()
	if site.statusExpired() || site.status == status {
		site.statusInterest++
		if site.statusInterest == 1 {
			go site.statusLoop()
		}
		go func() {
			lch := make(chan string)
			site.mu.Lock()
			for site.statusExpired() || site.status == status {
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

func LookupSite(s, host string) (*Site, error) {
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
	now := time.Now()
	if site == nil {
		site = new(Site)
		site.siteurl = siteurl
		site.createAt = now
		site.statusWaiters = make([]chan<- string, 0, 8)
		sitemap[siteurl] = site
	}
	site.accessAt = now
	sitemapmu.Unlock()

	return site, nil
}


type SiteResponse struct {
	Ok bool `json:"ok"`
	Error string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
}

func SiteRequest(w http.ResponseWriter, req *http.Request) {
	var result SiteResponse
	if site, err := LookupSite(req.FormValue("conference"), req.Host); err != nil {
		result.Error = err.Error()
	} else if poll := req.FormValue("poll"); poll != "" {
		status := <-site.DifferentStatus(poll)
		result.Message = "status " + status
		result.Ok = status != ""
	} else {
		status := <-site.Status()
		result.Message = "status " + status
		result.Ok = status != ""
	}
	data, _ := json.Marshal(result)
	w.Write(data)
}

func main() {
	http.HandleFunc("/", SiteRequest)
	log.Fatal(http.ListenAndServe("localhost:20444", nil))
}
