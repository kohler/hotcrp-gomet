package main

import (
    "encoding/json"
    "fmt"
    "io/ioutil"
    "log"
    "net/http"
    "sync"
    "time"
)

//const siteStatusTimeout = 120 * time.Second
const siteStatusTimeout = 10 * time.Second
const siteErrorStatusTimeout = 5 * time.Second


type TrackerSeq float64

func (seq TrackerSeq) MarshalJSON() ([]byte, error) {
    return []byte(fmt.Sprintf("%f", seq)), nil
}


type SiteStatus struct {
    Status string
    Seq TrackerSeq
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
        } else if site.statusInterest != 0 {
			go site.renewStatus()
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
    Seq TrackerSeq `json:"tracker_status_at"`
}

func (site *Site) renewStatus() {
    site.mu.Lock()
    if !site.statusRefreshing {
        site.statusRefreshing = true
        site.mu.Unlock()

        log.Printf("%sapi/trackerstatus -> ...\n", site.siteurl)
        resp, err := http.Get(site.siteurl + "api.php?fn=trackerstatus")
        var respbody []byte
        if err == nil {
            respbody, err = ioutil.ReadAll(resp.Body)
        }
        log.Printf("%sapi/trackerstatus -> %s\n", site.siteurl, string(respbody))
        statusResponse := TrackerStatusResponse{}
        if err == nil {
            err = json.Unmarshal(respbody, &statusResponse)
        }

        site.mu.Lock()
        newStatus := SiteStatus{"", 0, err}
        if err == nil && statusResponse.Ok {
            newStatus.Status = statusResponse.TrackerStatus
            newStatus.Seq = statusResponse.Seq
        } else if err == nil {
            newStatus.Error = fmt.Errorf("%s", statusResponse.Error)
        }
        site.Update(newStatus)
        site.statusRefreshing = false
    }
    site.mu.Unlock()
}

func (site *Site) Update(newStatus SiteStatus) {
    if newStatus.Error != nil || site.status.Status == "" || site.status.Seq < newStatus.Seq {
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

func (site *Site) statusDefinitelyDiffers(status SiteStatus) bool {
    return !site.statusExpired() &&
        status.Status != site.status.Status &&
        status.Seq <= site.status.Seq
}

func (site *Site) DifferentStatus(status SiteStatus, timeout time.Duration) <-chan SiteStatus {
    ch := make(chan SiteStatus, 1)
    site.mu.Lock()
    if site.statusDefinitelyDiffers(status) {
        ch <- site.status
    } else {
        site.statusInterest++
        if site.statusInterest == 1 {
            site.statusLooper <- struct{}{}
        }
        var timeoutch <-chan time.Time
        if timeout > 0 {
            timeoutch = time.After(timeout)
        }
        go func() {
            lch := make(chan SiteStatus, 1)
            timedout := false
            site.mu.Lock()
            for !site.statusDefinitelyDiffers(status) && !timedout {
                site.statusWaiters = append(site.statusWaiters, lch)
                site.mu.Unlock()
                select {
                case <-lch:
                    status.Seq = 0
                case <-timeoutch:
                    timedout = true
                }
                site.mu.Lock()
            }
            site.statusInterest--
            ch <- site.status
            site.mu.Unlock()
        }()
    }
    site.mu.Unlock()
    return ch
}
