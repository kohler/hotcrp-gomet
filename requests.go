package main

import (
    "encoding/json"
    "fmt"
    "net/http"
    "net/url"
    "strconv"
    "strings"
    "sync"
    "time"
)

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
    u.Host = strings.ToLower(u.Host)
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
    Seq TrackerSeq `json:"tracker_status_at,omitempty"`
    Error string `json:"error,omitempty"`
    Message string `json:"message,omitempty"`
}

func outputResponse(w http.ResponseWriter, result interface{}) {
    w.Header().Add("Access-Control-Allow-Origin", "*")
    w.Header().Add("Access-Control-Allow-Credentials", "true")
    w.Header().Add("Access-Control-Allow-Headers", "Accept-Encoding")
    w.Header().Add("Expires", "Mon, 26 Jul 1997 05:00:00 GMT")
    data, _ := json.Marshal(result)
    w.Write(data)
}

func PollRequest(w http.ResponseWriter, req *http.Request) {
    var result SiteResponse
    var status SiteStatus
    site, err := LookupSite(req.FormValue("conference"), req.Host, true)
    if err != nil {
        status = SiteStatus{Error: err}
    } else if poll := req.FormValue("poll"); poll != "" {
        status = SiteStatus{Status: poll}
        if x, err := strconv.ParseFloat(req.FormValue("tracker_status_at"), 64); err == nil {
            status.Seq = TrackerSeq(x)
        }
        var timeout time.Duration
        if x, err := strconv.ParseFloat(req.FormValue("timeout"), 64); err == nil {
            timeout = time.Duration(x * float64(time.Millisecond))
        }
        status = <-site.DifferentStatus(status, timeout)
    } else {
        status = <-site.Status()
    }
    if status.Error == nil {
        result.Ok = true
        result.Status = status.Status
        result.Seq = status.Seq
    } else {
        result.Error = status.Error.Error()
    }
    outputResponse(w, result)
}

func UpdateRequest(w http.ResponseWriter, req *http.Request) {
    fmt.Println(req.URL.String())
    var result SiteResponse
    site, err := LookupSite(req.FormValue("conference"), req.Host, true)
    if err != nil {
        result = SiteResponse{Ok: false, Error: err.Error()}
    } else if status := req.FormValue("tracker_status"); status != "" {
        status := SiteStatus{Status: status}
        if x, err := strconv.ParseFloat(req.FormValue("tracker_status_at"), 64); err == nil {
            status.Seq = TrackerSeq(x)
        }
        site.mu.Lock()
        site.Update(status)
        site.mu.Unlock()
        result = SiteResponse{Ok: true}
    } else {
        result = SiteResponse{Ok: false, Error: fmt.Sprintf("bad status update")}
    }
    outputResponse(w, result)
}
