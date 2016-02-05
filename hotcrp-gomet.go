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

const siteStatusTimeout = 120 * time.Second
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
	statusWaiters []chan<- string
}

func (site *Site) statusExpired() bool {
	timeout := time.Duration(siteStatusTimeout)
	if site.status == "" {
		timeout = time.Duration(siteErrorStatusTimeout)
	}
	return time.Now().After(site.statusSetAt.Add(timeout))
}

type TrackerStatusResponse struct {
	Ok bool `json:"ok"`
	Error string `json:"error"`
	TrackerStatus string `json:"tracker_status"`
	Sequencer float64 `json:"tracker_status_at"`
}

func (site *Site) Status(ch chan<- string) {
	site.mu.Lock()

	if !site.statusExpired() {
		ch <- site.status
		site.mu.Unlock()
		return
	}

	if site.statusWaiters != nil {
		site.statusWaiters = append(site.statusWaiters, ch)
		site.mu.Unlock()
		return
	}

	site.statusWaiters = make([]chan<- string, 0, 1)
	site.statusWaiters = append(site.statusWaiters, ch)
	apiurl := site.siteurl + "api.php?fn=trackerstatus"
	site.mu.Unlock()

	go func() {
		resp, err := http.Get(apiurl)
		var respbody []byte
		if err == nil {
			respbody, err = ioutil.ReadAll(resp.Body)
		}
		statusResponse := TrackerStatusResponse{}
		if err == nil {
			err = json.Unmarshal(respbody, &statusResponse)
		}
		site.mu.Lock()
		site.status = ""
		site.statusSetAt = time.Now()
		if err != nil {
			site.statusError = err
		} else if !statusResponse.Ok {
			site.statusError = fmt.Errorf("%s", statusResponse.Error)
		} else if statusResponse.Sequencer < site.statusSequencer {
			// ignore out-of-sequence response
		} else {
			site.status = statusResponse.TrackerStatus
			site.statusSequencer = statusResponse.Sequencer
		}
		for _, ch = range site.statusWaiters {
			ch <- site.status
		}
		site.statusWaiters = nil
		site.mu.Unlock()
	}()
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
	} else {
		ch := make(chan string)
		site.Status(ch)
		status := <-ch
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
