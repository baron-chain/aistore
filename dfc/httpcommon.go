// Package dfc provides distributed file-based cache with Amazon and Google Cloud backends.
/*
 * Copyright (c) 2017, NVIDIA CORPORATION. All rights reserved.
 *
 */
package dfc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io/ioutil"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/OneOfOne/xxhash"
	"github.com/golang/glog"
)

const (
	maxidleconns   = 20              // max num idle connections
	requesttimeout = 5 * time.Second // http timeout
)

//===========
//
// interfaces
//
//===========
type cloudif interface {
	listbucket(w http.ResponseWriter, bucket string, msg *GetMsg) (errstr string)
	getobj(fqn, bucket, objname string) (errstr string)
	putobj(r *http.Request, fqn, bucket, objname, md5sum string) (errstr string)
	deleteobj(bucket, objname string) (errstr string)
}

//===========
//
// generic bad-request http handler
//
//===========
func invalhdlr(w http.ResponseWriter, r *http.Request) {
	s := http.StatusText(http.StatusBadRequest)
	s += ": " + r.Method + " " + r.URL.Path + " from " + r.RemoteAddr
	glog.Errorln(s)
	http.Error(w, s, http.StatusBadRequest)
}

//===========================================================================
//
// http runner
//
//===========================================================================
type glogwriter struct {
}

func (r *glogwriter) Write(p []byte) (int, error) {
	n := len(p)
	s := string(p[:n])
	glog.Errorln(s)
	return n, nil
}

type httprunner struct {
	namedrunner
	mux        *http.ServeMux
	h          *http.Server
	glogger    *log.Logger
	si         *daemonInfo
	httpclient *http.Client // http client for intra-cluster comm
	statsif    statsif
}

func (r *httprunner) registerhdlr(path string, handler func(http.ResponseWriter, *http.Request)) {
	if r.mux == nil {
		r.mux = http.NewServeMux()
	}
	r.mux.HandleFunc(path, handler)
}

func (r *httprunner) init(s statsif) error {
	r.statsif = s
	ipaddr, err := getipaddr() // FIXME: this must change
	if err != nil {
		return err
	}
	// http client
	r.httpclient = &http.Client{
		Transport: &http.Transport{MaxIdleConnsPerHost: maxidleconns},
		Timeout:   requesttimeout,
	}
	// init daemonInfo here
	r.si = &daemonInfo{}
	r.si.NodeIPAddr = ipaddr
	r.si.DaemonPort = ctx.config.Listen.Port

	// NOTE: generate and assign ID and URL here
	split := strings.Split(ipaddr, ".")
	cs := xxhash.ChecksumString32S(split[len(split)-1], mLCG32)
	r.si.DaemonID = strconv.Itoa(int(cs&0xffff)) + ":" + ctx.config.Listen.Port
	r.si.DirectURL = "http://" + r.si.NodeIPAddr + ":" + r.si.DaemonPort
	return nil
}

func (r *httprunner) run() error {
	// a wrapper to glog http.Server errors - otherwise
	// os.Stderr would be used, as per golang.org/pkg/net/http/#Server
	r.glogger = log.New(&glogwriter{}, "net/http err: ", 0)

	portstring := ":" + ctx.config.Listen.Port
	r.h = &http.Server{Addr: portstring, Handler: r.mux, ErrorLog: r.glogger}
	if err := r.h.ListenAndServe(); err != nil {
		if err != http.ErrServerClosed {
			glog.Errorf("Terminated %s with err: %v", r.name, err)
			return err
		}
	}
	return nil
}

// stop gracefully
func (r *httprunner) stop(err error) {
	glog.Infof("Stopping %s, err: %v", r.name, err)

	contextwith, cancel := context.WithTimeout(context.Background(), ctx.config.HttpTimeout)
	defer cancel()

	err = r.h.Shutdown(contextwith)
	if err != nil {
		glog.Infof("Stopped %s, err: %v", r.name, err)
	}
}

// intra-cluster IPC, control plane
// http-REST calls another target or a proxy
// optionally, sends a json-encoded content to the callee
// expects only OK or FAIL in the return
func (r *httprunner) call(url string, method string, injson []byte) (outjson []byte, err error) {
	var (
		request  *http.Request
		response *http.Response
	)
	if injson == nil || len(injson) == 0 {
		request, err = http.NewRequest(method, url, nil)
		if glog.V(3) {
			glog.Infof("%s URL %q", method, url)
		}
	} else {
		request, err = http.NewRequest(method, url, bytes.NewBuffer(injson))
		if err == nil {
			request.Header.Set("Content-Type", "application/json")
		}
	}
	if err != nil {
		glog.Errorf("Unexpected failure to create http request %s %s, err: %v", method, url, err)
		return nil, err
	}
	response, err = r.httpclient.Do(request)
	if err != nil {
		glog.Errorf("Failed to execute http call(%s %s), err: %v", method, url, err)
		return nil, err
	}
	assert(response != nil, "Unexpected: nil response in presense of no error")

	// block until done (returned content is ignored and discarded)
	defer func() { err = response.Body.Close() }()
	if outjson, err = ioutil.ReadAll(response.Body); err != nil {
		glog.Errorf("Failed to read http, err: %v", err)
		return nil, err
	}
	return outjson, err
}

//=============================
//
// http request parsing helpers
//
//=============================
func (r *httprunner) restAPIItems(unescapedpath string, maxsplit int) []string {
	escaped := html.EscapeString(unescapedpath)
	split := strings.SplitN(escaped, "/", maxsplit)
	apitems := make([]string, 0, len(split))
	for i := 0; i < len(split); i++ {
		if split[i] != "" { // omit empty
			apitems = append(apitems, split[i])
		}
	}
	return apitems
}

// remove validated fields and return the resulting slice
func (h *httprunner) checkRestAPI(w http.ResponseWriter, r *http.Request, apitems []string, n int, ver, res string) []string {
	if len(apitems) > 0 && ver != "" {
		if apitems[0] != ver {
			s := fmt.Sprintf("Invalid API version: %s (expecting %s)", apitems[0], ver)
			h.invalmsghdlr(w, r, s)
			return nil
		}
		apitems = apitems[1:]
	}
	if len(apitems) > 0 && res != "" {
		if apitems[0] != res {
			s := fmt.Sprintf("Invalid API resource: %s (expecting %s)", apitems[0], res)
			h.invalmsghdlr(w, r, s)
			return nil
		}
		apitems = apitems[1:]
	}
	if len(apitems) < n {
		s := fmt.Sprintf("Invalid API request: num elements %d (expecting at least %d [%v])", len(apitems), n, apitems)
		h.invalmsghdlr(w, r, s)
		return nil
	}
	return apitems
}

func (h *httprunner) readJSON(w http.ResponseWriter, r *http.Request, out interface{}) error {
	b, err := ioutil.ReadAll(r.Body)
	errclose := r.Body.Close()
	if err == nil && errclose != nil {
		err = errclose
	}
	if err == nil {
		err = json.Unmarshal(b, out)
	}
	if err != nil {
		s := fmt.Sprintf("Failed to json-unmarshal %s request, err: %v [%v]", r.Method, err, string(b))
		h.invalmsghdlr(w, r, s)
		return err
	}
	return nil
}

//=================
//
// commong set config
//
//=================
func (h *httprunner) setconfig(name, value string) string {
	lm, hm := ctx.config.LRUConfig.LowWM, ctx.config.LRUConfig.HighWM
	checkwm := false
	atoi := func(value string) (uint32, error) {
		v, err := strconv.Atoi(value)
		return uint32(v), err
	}
	switch name {
	case "stats_time":
		if v, err := time.ParseDuration(value); err != nil {
			return fmt.Sprintf("Failed to parse stats_time, err: %v", err)
		} else {
			ctx.config.StatsTime, ctx.config.StatsTimeStr = v, value
		}
	case "dont_evict_time":
		if v, err := time.ParseDuration(value); err != nil {
			return fmt.Sprintf("Failed to parse dont_evict_time, err: %v", err)
		} else {
			ctx.config.LRUConfig.DontEvictTime, ctx.config.LRUConfig.DontEvictTimeStr = v, value
		}
	case "lowwm":
		if v, err := atoi(value); err != nil {
			return fmt.Sprintf("Failed to convert lowwm, err: %v", err)
		} else {
			ctx.config.LRUConfig.LowWM, checkwm = v, true
		}
	case "highwm":
		if v, err := atoi(value); err != nil {
			return fmt.Sprintf("Failed to convert highwm, err: %v", err)
		} else {
			ctx.config.LRUConfig.HighWM, checkwm = v, true
		}
	case "no_xattrs":
		if v, err := strconv.ParseBool(value); err != nil {
			return fmt.Sprintf("Failed to parse no_xattrs, err: %v", err)
		} else {
			ctx.config.NoXattrs = v
		}
	case "passthru":
		if v, err := strconv.ParseBool(value); err != nil {
			return fmt.Sprintf("Failed to parse passthru (proxy-only), err: %v", err)
		} else {
			ctx.config.Proxy.Passthru = v
		}
	default:
		return fmt.Sprintf("Cannot set config var %s - readonly or unsupported", name)
	}
	if checkwm {
		hwm, lwm := ctx.config.LRUConfig.HighWM, ctx.config.LRUConfig.LowWM
		if hwm <= 0 || lwm <= 0 || hwm < lwm || lwm > 100 || hwm > 100 {
			ctx.config.LRUConfig.LowWM, ctx.config.LRUConfig.HighWM = lm, hm
			return fmt.Sprintf("Invalid LRU watermarks %+v", ctx.config.LRUConfig)
		}
	}
	return ""
}

//=================
//
// http err + spec message + code + stats
//
//=================
func (h *httprunner) invalmsghdlr(w http.ResponseWriter, r *http.Request, specific string, other ...interface{}) {
	s := http.StatusText(http.StatusBadRequest) + ": " + specific
	s += ": " + r.Method + " " + r.URL.Path + " from " + r.RemoteAddr
	glog.Errorln(s)
	glog.Flush()
	status := http.StatusBadRequest
	if len(other) > 0 {
		status = other[0].(int)
	}
	http.Error(w, s, status)
	h.statsif.add("numerr", 1)
}
