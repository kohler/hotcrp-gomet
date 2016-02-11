// +build linux

package main

import (
	"encoding/json"
	"golang.org/x/exp/inotify"
	"io/ioutil"
	"log"
	"os"
)

func init() {
	directoryWatcher = DirectoryWatcher
}

func DirectoryWatcher(dir string) {
	watcher, err := inotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	err = watcher.AddWatch(dir, inotify.IN_CLOSE_WRITE)
	if err != nil {
		log.Fatal(err)
	}
	for {
		select {
		case ev := <-watcher.Event:
			directoryWatcherFile(dir, ev.Name)
		case err := <-watcher.Error:
			log.Printf("directory watch: %s", err)
		}
	}
}


type TrackerStatusUpdate struct {
	Conference string `json:"conference"`
	TrackerStatus string `json:"tracker_status"`
	Sequencer TrackerSequencer `json:"tracker_status_at"`
}

func directoryWatcherFile(dir, name string) {
	if name[0:1] != "/" {
		log.Printf("directory watch: %s: unexpected name", name)
		return
	}

	content, err := ioutil.ReadFile(name)
	if err != nil {
		log.Printf("directory watch: %s", err)
		return
	}

	var tsu TrackerStatusUpdate
	err = json.Unmarshal(content, &tsu)
	if err != nil {
		log.Printf("directory watch: %s: %s", name, err)
		return
	}

	site, err := LookupSite(tsu.Conference, "", false)
	if err != nil {
		log.Printf("directory watch: %s: %s", name, err)
		return
	} else if tsu.TrackerStatus == "" || tsu.Sequencer <= 0 {
		log.Printf("directory watch: %s: bad status update", name)
		return
	}

	if site != nil {
		site.mu.Lock()
		site.Update(SiteStatus{Status: tsu.TrackerStatus, Sequencer: tsu.Sequencer})
		site.mu.Unlock()
	}

	os.Remove(name)
}
