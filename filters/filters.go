package filters

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/AdguardTeam/AdGuardHome/util"
	"github.com/AdguardTeam/golibs/file"
	"github.com/AdguardTeam/golibs/log"
)

const (
	// EventBeforeUpdate - this event is signalled before the update procedure renames/removes old filter files
	EventBeforeUpdate = iota
	// EventAfterUpdate - this event is signalled after the update procedure is finished
	EventAfterUpdate
)

// EventHandler - event handler function
type EventHandler func(flags uint)

// Filter - filter object
type Filter struct {
	ID      uint64 `yaml:"id"`
	Enabled bool   `yaml:"enabled"`
	Name    string `yaml:"name"`
	URL     string `yaml:"url"`

	Path string `yaml:"-"`

	RuleCount   uint64    `yaml:"-"`
	LastUpdated time.Time `yaml:"-"`
	newID       uint64
	nextUpdate  time.Time
}

// Conf - configuration
type Conf struct {
	FilterDir           string
	UpdateIntervalHours uint32
	HTTPClient          *http.Client
	Proxylist           []Filter
}

// Filters - module object
type Filters struct {
	filtersUpdated    bool
	updateTaskRunning bool
	conf              Conf
	confLock          sync.Mutex

	Users []EventHandler
}

// Init - initialize the module
func (fs *Filters) Init(conf Conf) {
	fs.conf = conf
}

// Start - start module
func (fs *Filters) Start() {
	for i := range fs.conf.Proxylist {
		f := &fs.conf.Proxylist[i]
		fname := fs.filterPath(*f)
		st, err := os.Stat(fname)
		if err != nil {
			log.Error("Filters: os.Stat: %s %s", fname, err)
			continue
		}
		f.LastUpdated = st.ModTime()
		f.nextUpdate = f.LastUpdated.Add(time.Duration(fs.conf.UpdateIntervalHours) * time.Hour)

		body, err := ioutil.ReadFile(fname)
		if err != nil {
			log.Error("Filters: ioutil.ReadFile: %s %s", fname, err)
			continue
		}
		_ = parseFilter(f, body)
	}

	if !fs.updateTaskRunning {
		fs.updateTaskRunning = true
		go fs.updateFilters()
	}
}

// Close - close the module
func (fs *Filters) Close() {
}

// Duplicate filter array
func arrayFilterDup(f []Filter) []Filter {
	nf := make([]Filter, len(f))
	copy(nf, f)
	return nf
}

// WriteDiskConfig - write configuration on disk
func (fs *Filters) WriteDiskConfig(c *Conf) {
	fs.confLock.Lock()
	*c = fs.conf
	c.Proxylist = arrayFilterDup(fs.conf.Proxylist)
	fs.confLock.Unlock()
}

// AddUser - add user handler for notifications
func (fs *Filters) AddUser(handler EventHandler) {
	fs.Users = append(fs.Users, handler)
}

// NotifyUsers - notify all users about the event
func (fs *Filters) NotifyUsers(flags uint) {
	for _, u := range fs.Users {
		u(flags)
	}
}

// List (thread safe)
func (fs *Filters) List(flags uint) []Filter {
	fs.confLock.Lock()
	ff := make([]Filter, len(fs.conf.Proxylist))
	for _, f := range fs.conf.Proxylist {
		nf := f
		nf.Path = fs.filterPath(f)
		ff = append(ff, nf)
	}
	fs.confLock.Unlock()
	return ff
}

// Get filter file name
func (fs *Filters) filterPath(f Filter) string {
	return filepath.Join(fs.conf.FilterDir, fmt.Sprintf("%d.txt", f.ID))
}

// Get next filter ID
func (fs *Filters) nextFilterID() uint64 {
	return uint64(time.Now().Unix())
}

// Download data via HTTP
func download(client *http.Client, url string) ([]byte, error) {
	resp, err := client.Get(url)
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != 200 {
		err := fmt.Errorf("status code: %d", resp.StatusCode)
		return nil, err
	}

	return ioutil.ReadAll(resp.Body)
}

// Parse filter data
func parseFilter(f *Filter, body []byte) error {
	data := string(body)
	rulesCount := 0

	// Count lines in the filter
	for len(data) != 0 {
		line := util.SplitNext(&data, '\n')
		if len(line) == 0 ||
			line[0] == '#' ||
			line[0] == '!' {
			continue
		}

		rulesCount++
	}

	f.RuleCount = uint64(rulesCount)
	return nil
}

// Download filter data
func (fs *Filters) downloadFilter(f *Filter) error {
	log.Debug("Filters: Downloading filter from %s", f.URL)

	body, err := download(fs.conf.HTTPClient, f.URL)
	if err != nil {
		err := fmt.Errorf("Filters: Couldn't download filter from %s: %s", f.URL, err)
		return err
	}

	err = parseFilter(f, body)
	if err != nil {
		return err
	}

	fname := fs.filterPath(*f)
	err = file.SafeWrite(fname, body)
	if err != nil {
		return err
	}

	log.Debug("Filters: saved filter %s at %s", f.URL, fname)
	f.LastUpdated = time.Now()
	return nil
}

// Add - add filter (thread safe)
func (fs *Filters) Add(nf Filter) error {
	fs.confLock.Lock()
	defer fs.confLock.Unlock()

	for _, f := range fs.conf.Proxylist {
		if f.Name == nf.Name || f.URL == nf.URL {
			return fmt.Errorf("filter with this Name or URL already exists")
		}
	}

	nf.ID = fs.nextFilterID()
	nf.Enabled = true
	err := fs.downloadFilter(&nf)
	if err != nil {
		log.Debug("%s", err)
		return err
	}
	fs.conf.Proxylist = append(fs.conf.Proxylist, nf)
	log.Debug("Filters: added filter %s", nf.URL)
	return nil
}

// Delete - remove filter (thread safe)
func (fs *Filters) Delete(url string) *Filter {
	fs.confLock.Lock()
	defer fs.confLock.Unlock()

	nf := []Filter{}
	var found *Filter
	for _, f := range fs.conf.Proxylist {
		if f.URL == url {
			found = &f
			continue
		}
		nf = append(nf, f)
	}
	if found == nil {
		return nil
	}
	fs.conf.Proxylist = nf
	log.Debug("Filters: removed filter %s", url)
	found.Path = fs.filterPath(*found) // the caller will delete the file
	return found
}

const (
	// StatusNotFound - NotFound
	StatusNotFound = 1
	// StatusChangedEnabled - ChangedEnabled
	StatusChangedEnabled = 2
	// StatusChangedURL - ChangedURL
	StatusChangedURL = 4
)

// Modify - set filter properties (thread safe)
// Return Status* bitarray
func (fs *Filters) Modify(url string, enabled bool, name string, newURL string) int {
	fs.confLock.Lock()
	defer fs.confLock.Unlock()

	st := 0

	for _, f := range fs.conf.Proxylist {
		if f.URL == url {

			f.Name = name

			if f.Enabled != enabled {
				f.Enabled = enabled
				st |= StatusChangedEnabled
			}

			if f.URL != newURL {
				f.URL = newURL
				st |= StatusChangedURL
			}

			break
		}
	}

	if st == 0 {
		return StatusNotFound
	}

	return st
}

func (fs *Filters) Refresh(flags uint) {
	// TODO
}

// Periodically update filters
// Algorithm:
// . Get next filter to update:
//  . Download data from Internet and store on disk (in a new file)
//  . Update filter's properties
//  . Repeat for next filter
// (All filters are downloaded)
// . Stop modules that use filters
// . Rename "new file name" -> "old file name"
// . Restart modules that use filters
func (fs *Filters) updateFilters() {
	period := time.Hour
	for {
		// if !dns.isRunning()
		//  sleep

		fs.confLock.Lock()
		f := fs.getNextToUpdate()
		uf := *f
		fs.confLock.Unlock()

		if f == nil {
			fs.applyUpdate()

			time.Sleep(period)
			continue
		}

		uf.ID = fs.nextFilterID()
		err := fs.downloadFilter(&uf)
		if err != nil {
			continue
		}

		fs.confLock.Lock()
		fs.modifyUpdated(uf)
		fs.confLock.Unlock()
	}
}

// Get next filter to update
func (fs *Filters) getNextToUpdate() *Filter {
	now := time.Now()

	for i := range fs.conf.Proxylist {
		f := &fs.conf.Proxylist[i]

		if f.Enabled &&
			f.nextUpdate.Unix() <= now.Unix() {

			f.nextUpdate = now.Add(time.Duration(fs.conf.UpdateIntervalHours) * time.Hour)
			return f
		}
	}

	return nil
}

// Set new filter properties after update
func (fs *Filters) modifyUpdated(uf Filter) {
	for i := range fs.conf.Proxylist {
		f := &fs.conf.Proxylist[i]

		if f.URL == uf.URL {
			f.newID = uf.ID
			f.RuleCount = uf.RuleCount
			f.LastUpdated = uf.LastUpdated

			fs.filtersUpdated = true
			break
		}
	}
}

// Replace filter files
func (fs *Filters) applyUpdate() {
	if !fs.filtersUpdated {
		log.Debug("Filters: no filters were updated")
		return
	}
	fs.filtersUpdated = false

	fs.NotifyUsers(EventBeforeUpdate)

	nUpdated := 0

	fs.confLock.Lock()
	for i := range fs.conf.Proxylist {
		f := &fs.conf.Proxylist[i]

		if f.newID != 0 && f.newID != f.ID {
			name := fs.filterPath(*f)
			newName := fs.filterPath(Filter{ID: f.newID})
			err := os.Rename(newName, name)
			if err != nil {
				log.Error("Filters: os.Rename:%s", err)
			}
			f.newID = 0
			nUpdated++
		}
	}
	fs.confLock.Unlock()

	log.Debug("Filters: %d filters were updated", nUpdated)

	fs.NotifyUsers(EventAfterUpdate)
}
