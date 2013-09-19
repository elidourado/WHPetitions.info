package whpetitionsinfo

import (
	"appengine"
	"appengine/datastore"
	"appengine/memcache"
	"appengine/urlfetch"
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io/ioutil"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Stats struct {
	AverageDuration time.Duration
	Number          int
}

type RenderStats struct {
	AverageResponse, AveragePending, NumberResponse, NumberPending, NumberTotal, PercentResponded int
}

type RenderData struct {
	Stats     RenderStats
	Petitions PetitionSet
}

type APIResponse struct {
	Results []Petition
}

type Petition struct {
	Id, Title, Url, Status                                                  string
	Body                                                                    string `datastore:",noindex"`
	SignatureThreshold, SignatureCount, SignaturesNeeded, Deadline, Created int
	Response                                                                WHResponse
	DeadlineTime, UpdatedTime                                               time.Time
	YearAgo                                                                 bool
}

type WHResponse struct {
	Id, Url, AssociationTime string
}

type PetitionSet []Petition

func init() {
	http.HandleFunc("/", mainHandler)
	http.HandleFunc("/updatePending", pendingHandler)
	http.HandleFunc("/updateResponded", respondedHandler)
}

/*

	TEMPLATE STUFF

*/

var templateFuncs = template.FuncMap{
	"addCommas": Comma,
}

var index = template.Must(template.New("base.html").Funcs(templateFuncs).ParseFiles(
	"templates/base.html",
	"templates/petitions.html",
))

var page404 = template.Must(template.ParseFiles(
	"templates/base.html",
	"templates/404.html",
))

var empty = template.Must(template.ParseFiles(
	"templates/base.html",
	"templates/empty.html",
))

/*
	HANDLERS
*/

func mainHandler(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	if r.Method != "GET" || r.URL.Path != "/" {
		w.WriteHeader(http.StatusNotFound)
		if item, err := memcache.Get(c, "cached404"); err == memcache.ErrCacheMiss {
			// Item not in cache; continue.
		} else if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else {
			w.Write(item.Value)
			return
		}
	} else {
		if item, err := memcache.Get(c, "cachedIndex"); err == memcache.ErrCacheMiss {
			// Item not in cache; continue.
		} else if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else {
			w.Write(item.Value)
			return
		}
	}

	var renderData RenderData
	var stats RenderStats
	var response APIResponse
	if err8 := datastore.Get(c, datastore.NewKey(c, "APIResponse", "pending slice", 0, nil), &response); err8 != nil {
		http.Error(w, err8.Error(), http.StatusInternalServerError)
		return
	}
	petitions := response.Results

	var responseStats Stats
	var pendingStats Stats
	if err6 := datastore.Get(c, datastore.NewKey(c, "Stats", "responded", 0, nil), &responseStats); err6 != nil {
		http.Error(w, err6.Error(), http.StatusInternalServerError)
		return
	}
	if err7 := datastore.Get(c, datastore.NewKey(c, "Stats", "pending response", 0, nil), &pendingStats); err7 != nil {
		http.Error(w, err7.Error(), http.StatusInternalServerError)
		return
	}

	stats.AveragePending = int(math.Floor(pendingStats.AverageDuration.Hours() / 24))
	stats.AverageResponse = int(math.Floor(responseStats.AverageDuration.Hours() / 24))
	stats.NumberResponse = responseStats.Number
	stats.NumberPending = pendingStats.Number
	stats.NumberTotal = stats.NumberResponse + stats.NumberPending
	stats.PercentResponded = int(math.Floor(100 * float64(stats.NumberResponse) / float64(stats.NumberTotal)))

	renderData.Stats = stats
	renderData.Petitions = petitions

	b := bytes.NewBufferString("")

	if r.Method != "GET" || r.URL.Path != "/" {
		if err := page404.Execute(b, renderData); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		item := &memcache.Item{
			Key:        "cached404",
			Value:      b.Bytes(),
			Expiration: time.Duration(90) * time.Minute, // 90 minutes, but note that the data updaters flush memcache
		}
		if err9 := memcache.Set(c, item); err9 != nil {
			c.Errorf("error setting item: %v", err9)
		}
		b.WriteTo(w)
		return
	} else {
		if len(petitions) == 0 {
			if err := empty.Execute(b, renderData); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		} else {
			if err := index.Execute(b, renderData); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		item := &memcache.Item{
			Key:        "cachedIndex",
			Value:      b.Bytes(),
			Expiration: time.Duration(90) * time.Minute, // 90 minutes, but note that the data updaters flush memcache
		}
		if err9 := memcache.Set(c, item); err9 != nil {
			c.Errorf("error setting item: %v", err9)
		}
		b.WriteTo(w)
		return
	}
}

func pendingHandler(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	url := "https://api.whitehouse.gov/v1/petitions.json?status=pending%20response&limit=500"
	response, err := getJSON(url, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var stats Stats
	var total time.Duration = 0
	now := time.Now()
	petitions := response.Results
	stats.Number = len(petitions)
	sort.Sort(PetitionSet(petitions))
	for item := range petitions {
		petitions[item].DeadlineTime = time.Unix(int64(petitions[item].Deadline), 0)
		petitions[item].UpdatedTime = now
		diff := now.Sub(petitions[item].DeadlineTime)
		total += diff
		if diff.Hours() > 24*365 {
			petitions[item].YearAgo = true
		}
	}
	_, err3 := datastore.Put(c, datastore.NewKey(c, "APIResponse", "pending slice", 0, nil), &response)
	if err3 != nil {
		http.Error(w, err3.Error(), http.StatusInternalServerError)
		return
	}
	stats.AverageDuration = time.Duration(float64(total) / float64(stats.Number))
	_, err4 := datastore.Put(c, datastore.NewKey(c, "Stats", "pending response", 0, nil), &stats)
	if err4 != nil {
		http.Error(w, err4.Error(), http.StatusInternalServerError)
		return
	}
	memcache.Flush(c)
	fmt.Fprint(w, "OK")
}

func respondedHandler(w http.ResponseWriter, r *http.Request) {
	c := appengine.NewContext(r)
	url := "https://api.whitehouse.gov/v1/petitions.json?status=responded&limit=500"
	response, err := getJSON(url, r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	petitions := response.Results

	var stats Stats
	stats.Number = len(petitions)
	totalNumber := stats.Number
	var total float64 = 0
	for i := range petitions {
		replyTime, err5 := strconv.ParseInt(petitions[i].Response.AssociationTime, 10, 64)
		if err5 == nil {
			total += float64(replyTime) - float64(petitions[i].Deadline)
		} else {
			totalNumber -= 1
		}
	}
	average := total / float64(totalNumber)
	stats.AverageDuration = time.Duration(average * 1e9)
	datastore.Put(c, datastore.NewKey(c, "Stats", "responded", 0, nil), &stats)
	memcache.Flush(c)
	fmt.Fprint(w, "OK")
}

/*

	OTHER FUNCTIONS

*/

func getJSON(url string, r *http.Request) (APIResponse, error) {
	c := appengine.NewContext(r)

	transport := urlfetch.Transport{
		Context:                       c,
		Deadline:                      time.Duration(20) * time.Second,
		AllowInvalidServerCertificate: false,
	}
	req, err0 := http.NewRequest("GET", url, nil)
	if err0 != nil {
		return APIResponse{}, err0
	}
	resp, err1 := transport.RoundTrip(req)
	if err1 != nil {
		return APIResponse{}, err1
	}
	body, err2 := ioutil.ReadAll(resp.Body)
	if err2 != nil {
		return APIResponse{}, err2
	}
	resp.Body.Close()
	var f APIResponse
	err3 := json.Unmarshal(body, &f)
	if err3 != nil {
		return APIResponse{}, err3
	}
	return f, nil
}

func Comma(v int) string {
	sign := ""
	if v < 0 {
		sign = "-"
		v = 0 - v
	}
	parts := []string{"", "", "", "", "", "", "", "", ""}
	j := len(parts) - 1
	for v > 999 {
		parts[j] = strconv.FormatInt(int64(v)%1000, 10)
		switch len(parts[j]) {
		case 2:
			parts[j] = "0" + parts[j]
		case 1:
			parts[j] = "00" + parts[j]
		}
		v = v / 1000
		j--
	}
	parts[j] = strconv.Itoa(int(v))
	return sign + strings.Join(parts[j:len(parts)], ",")
}

// Sorting

func (s PetitionSet) Len() int {
	return len(s)
}
func (s PetitionSet) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}
func (s PetitionSet) Less(i, j int) bool {
	return s[i].Deadline < s[j].Deadline
}
