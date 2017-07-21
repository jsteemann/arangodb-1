//
// DISCLAIMER
//
// Copyright 2017 ArangoDB GmbH, Cologne, Germany
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Copyright holder is ArangoDB GmbH, Cologne, Germany
//
// Author Ewout Prangsma
//

package service

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"github.com/arangodb-helper/arangodb/client"
	logging "github.com/op/go-logging"
)

var (
	httpClient = client.DefaultHTTPClient()
)

const (
	contentTypeJSON = "application/json"
)

// HelloRequest is the data structure send of the wire in a `/hello` POST request.
type HelloRequest struct {
	SlaveID      string // Unique ID of the slave
	SlaveAddress string // IP address used to reach the slave (if empty, this will be derived from the request)
	SlavePort    int    // Port used to reach the slave
	DataDir      string // Directory used for data by this slave
	IsSecure     bool   // If set, servers started by this peer are using an SSL connection
	Agent        *bool  `json:",omitempty"` // If not nil, sets if server gets an agent or not. If nil, default handling applies
	DBServer     *bool  `json:",omitempty"` // If not nil, sets if server gets an dbserver or not. If nil, default handling applies
	Coordinator  *bool  `json:",omitempty"` // If not nil, sets if server gets an coordinator or not. If nil, default handling applies
}

type GoodbyeRequest struct {
	SlaveID string // Unique ID of the slave that should be removed.
}

type httpServer struct {
	//config Config
	log                  *logging.Logger
	context              httpServerContext
	versionInfo          client.VersionInfo
	idInfo               client.IDInfo
	runtimeServerManager *runtimeServerManager
	masterPort           int
}

// httpServerContext provides a context for the httpServer.
type httpServerContext interface {
	// ClusterConfig returns the current cluster configuration and the current peer
	ClusterConfig() (ClusterConfig, *Peer, ServiceMode)

	// IsRunningMaster returns if the starter is the running master.
	IsRunningMaster() (isRunningMaster, isRunning bool, masterURL string)

	// serverHostDir returns the path of the folder (in host namespace) containing data for the given server.
	serverHostDir(serverType ServerType) (string, error)

	// sendMasterLeaveCluster informs the master that we're leaving for good.
	// The master will remove the database servers from the cluster and update
	// the cluster configuration.
	sendMasterLeaveCluster() error

	// Stop the peer
	Stop()

	// Handle a hello request.
	// If req==nil, this is a GET request, otherwise it is a POST request.
	HandleHello(ownAddress, remoteAddress string, req *HelloRequest, isUpdateRequest bool) (ClusterConfig, error)

	// HandleGoodbye removes the database servers started by the peer with given id
	// from the cluster and alters the cluster configuration, removing the peer.
	HandleGoodbye(id string) (peerRemoved bool, err error)

	// Called by an agency callback
	MasterChangedCallback()
}

// newHTTPServer initializes and an HTTP server.
func newHTTPServer(log *logging.Logger, context httpServerContext, runtimeServerManager *runtimeServerManager, config Config, serverID string) *httpServer {
	// Create HTTP server
	return &httpServer{
		log:     log,
		context: context,
		idInfo: client.IDInfo{
			ID: serverID,
		},
		versionInfo: client.VersionInfo{
			Version: config.ProjectVersion,
			Build:   config.ProjectBuild,
		},
		runtimeServerManager: runtimeServerManager,
		masterPort:           config.MasterPort,
	}
}

// Start listening for requests.
// This method will return directly after starting.
func (s *httpServer) Start(hostAddr, containerAddr string, tlsConfig *tls.Config) {
	mux := http.NewServeMux()
	// Starter to starter API
	mux.HandleFunc("/hello", s.helloHandler)
	mux.HandleFunc("/goodbye", s.goodbyeHandler)
	// External API
	mux.HandleFunc("/id", s.idHandler)
	mux.HandleFunc("/process", s.processListHandler)
	mux.HandleFunc("/endpoints", s.endpointsHandler)
	mux.HandleFunc("/logs/agent", s.agentLogsHandler)
	mux.HandleFunc("/logs/dbserver", s.dbserverLogsHandler)
	mux.HandleFunc("/logs/coordinator", s.coordinatorLogsHandler)
	mux.HandleFunc("/logs/single", s.singleLogsHandler)
	mux.HandleFunc("/version", s.versionHandler)
	mux.HandleFunc("/shutdown", s.shutdownHandler)
	// Agency callback
	mux.HandleFunc("/cb/masterChanged", s.cbMasterChanged)

	go func() {
		server := &http.Server{
			Addr:    containerAddr,
			Handler: mux,
		}
		if tlsConfig != nil {
			s.log.Infof("Listening on %s (%s) using TLS", containerAddr, hostAddr)
			server.TLSConfig = tlsConfig
			if err := server.ListenAndServeTLS("", ""); err != nil {
				s.log.Errorf("Failed to listen on %s: %v", containerAddr, err)
			}
		} else {
			s.log.Infof("Listening on %s (%s)", containerAddr, hostAddr)
			if err := server.ListenAndServe(); err != nil {
				s.log.Errorf("Failed to listen on %s: %v", containerAddr, err)
			}
		}
	}()
}

// HTTP service function:

func (s *httpServer) helloHandler(w http.ResponseWriter, r *http.Request) {
	s.log.Debugf("Received %s /hello request from %s", r.Method, r.RemoteAddr)

	// Derive own address
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Cannot derive own host address: %v", err))
		return
	}
	ownAddress := normalizeHostName(host)
	isUpdateRequest, _ := strconv.ParseBool(r.FormValue("update"))

	var result ClusterConfig
	if r.Method == "GET" {
		// Let service handle get request
		result, err = s.context.HandleHello(ownAddress, r.RemoteAddr, nil, isUpdateRequest)
		if err != nil {
			handleError(w, err)
			return
		}
	} else if r.Method == "POST" {
		// Read request
		var req HelloRequest
		defer r.Body.Close()
		if body, err := ioutil.ReadAll(r.Body); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("Cannot read request body: %v", err.Error()))
			return
		} else if err := json.Unmarshal(body, &req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("Cannot parse request body: %v", err.Error()))
			return
		}

		// Let service handle post request
		result, err = s.context.HandleHello(ownAddress, r.RemoteAddr, &req, false)
		if err != nil {
			handleError(w, err)
			return
		}
	} else {
		// Invalid method
		writeError(w, http.StatusMethodNotAllowed, "GET or POST required")
		return
	}

	// Send result
	b, err := json.Marshal(result)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
	} else {
		w.Write(b)
	}
}

// goodbyeHandler handles a `/goodbye` request that removes a peer from the list of peers.
func (s *httpServer) goodbyeHandler(w http.ResponseWriter, r *http.Request) {
	// Check method
	if r.Method != "POST" {
		writeError(w, http.StatusMethodNotAllowed, "POST required")
		return
	}

	var req GoodbyeRequest
	defer r.Body.Close()
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Cannot read request body: %v", err.Error()))
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("Cannot parse request body: %v", err.Error()))
		return
	}

	// Check request
	if req.SlaveID == "" {
		writeError(w, http.StatusBadRequest, "SlaveID must be set.")
		return
	}

	// Remove the peer
	s.log.Infof("Goodbye requested for peer %s", req.SlaveID)
	if removed, err := s.context.HandleGoodbye(req.SlaveID); err != nil {
		// Failure
		handleError(w, err)
	} else if !removed {
		// ID not found
		writeError(w, http.StatusNotFound, "Unknown ID")
	} else {
		// Peer removed
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("BYE"))
	}
}

// idHandler returns a JSON object containing the ID of this starter.
func (s *httpServer) idHandler(w http.ResponseWriter, r *http.Request) {
	data, err := json.Marshal(s.idInfo)
	if err != nil {
		s.log.Errorf("Failed to marshal ID response: %#v", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(err.Error()))
	} else {
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	}
}

// processListHandler returns process information of all launched servers.
func (s *httpServer) processListHandler(w http.ResponseWriter, r *http.Request) {
	clusterConfig, myPeer, mode := s.context.ClusterConfig()
	isSecure := clusterConfig.IsSecure()

	// Gather processes
	resp := client.ProcessList{}
	expectedServers := 0
	if myPeer != nil {
		portOffset := myPeer.PortOffset
		ip := myPeer.Address
		if myPeer.HasAgent() {
			expectedServers++
		}
		if myPeer.HasDBServer() {
			expectedServers++
		}
		if myPeer.HasCoordinator() {
			expectedServers++
		}

		createServerProcess := func(serverType ServerType, p Process) client.ServerProcess {
			return client.ServerProcess{
				Type:        client.ServerType(serverType),
				IP:          ip,
				Port:        s.masterPort + portOffset + serverType.PortOffset(),
				ProcessID:   p.ProcessID(),
				ContainerID: p.ContainerID(),
				ContainerIP: p.ContainerIP(),
				IsSecure:    isSecure,
			}
		}

		if p := s.runtimeServerManager.agentProc; p != nil {
			resp.Servers = append(resp.Servers, createServerProcess(ServerTypeAgent, p))
		}
		if p := s.runtimeServerManager.coordinatorProc; p != nil {
			resp.Servers = append(resp.Servers, createServerProcess(ServerTypeCoordinator, p))
		}
		if p := s.runtimeServerManager.dbserverProc; p != nil {
			resp.Servers = append(resp.Servers, createServerProcess(ServerTypeDBServer, p))
		}
		if p := s.runtimeServerManager.singleProc; p != nil {
			resp.Servers = append(resp.Servers, createServerProcess(ServerTypeSingle, p))
		}
	}
	if mode.IsSingleMode() {
		expectedServers = 1
	}
	resp.ServersStarted = len(resp.Servers) == expectedServers
	b, err := json.Marshal(resp)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
	} else {
		w.Write(b)
	}
}

func urlListToStringSlice(list []url.URL) []string {
	result := make([]string, len(list))
	for i, u := range list {
		result[i] = u.String()
	}
	return result
}

// endpointsHandler returns the URL's needed to reach all starters, agents & coordinators in the cluster.
func (s *httpServer) endpointsHandler(w http.ResponseWriter, r *http.Request) {
	// IsRunningMaster returns if the starter is the running master.
	isRunningMaster, isRunning, masterURL := s.context.IsRunningMaster()

	// Check state
	if isRunning && !isRunningMaster {
		// Redirect to master
		if masterURL != "" {
			location, err := getURLWithPath(masterURL, "/endpoints")
			if err != nil {
				handleError(w, err)
			} else {
				handleError(w, RedirectError{Location: location})
			}
		} else {
			writeError(w, http.StatusServiceUnavailable, "No runtime master known")
		}
	} else {
		// Gather & send endpoints list
		clusterConfig, _, _ := s.context.ClusterConfig()

		// Gather endpoints
		resp := client.EndpointList{}
		if endpoints, err := clusterConfig.GetPeerEndpoints(); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		} else {
			resp.Starters = urlListToStringSlice(endpoints)
		}
		if isRunning {
			if endpoints, err := clusterConfig.GetAgentEndpoints(); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			} else {
				resp.Agents = urlListToStringSlice(endpoints)
			}
			if endpoints, err := clusterConfig.GetCoordinatorEndpoints(); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			} else {
				resp.Coordinators = urlListToStringSlice(endpoints)
			}
		}

		b, err := json.Marshal(resp)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
		} else {
			w.Write(b)
		}
	}
}

// agentLogsHandler servers the entire agent log (if any).
// If there is no agent running a 404 is returned.
func (s *httpServer) agentLogsHandler(w http.ResponseWriter, r *http.Request) {
	_, myPeer, _ := s.context.ClusterConfig()

	if myPeer != nil && myPeer.HasAgent() {
		s.logsHandler(w, r, ServerTypeAgent)
	} else {
		w.WriteHeader(http.StatusNotFound)
	}
}

// dbserverLogsHandler servers the entire dbserver log.
func (s *httpServer) dbserverLogsHandler(w http.ResponseWriter, r *http.Request) {
	_, myPeer, _ := s.context.ClusterConfig()

	if myPeer != nil && myPeer.HasDBServer() {
		s.logsHandler(w, r, ServerTypeDBServer)
	} else {
		w.WriteHeader(http.StatusNotFound)
	}
}

// coordinatorLogsHandler servers the entire coordinator log.
func (s *httpServer) coordinatorLogsHandler(w http.ResponseWriter, r *http.Request) {
	_, myPeer, _ := s.context.ClusterConfig()

	if myPeer != nil && myPeer.HasCoordinator() {
		s.logsHandler(w, r, ServerTypeCoordinator)
	} else {
		w.WriteHeader(http.StatusNotFound)
	}
}

// singleLogsHandler servers the entire single server log.
func (s *httpServer) singleLogsHandler(w http.ResponseWriter, r *http.Request) {
	s.logsHandler(w, r, ServerTypeSingle)
}

func (s *httpServer) logsHandler(w http.ResponseWriter, r *http.Request, serverType ServerType) {
	// Find log path
	myHostDir, err := s.context.serverHostDir(serverType)
	if err != nil {
		// Not ready yet
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	logPath := filepath.Join(myHostDir, logFileName)
	s.log.Debugf("Fetching logs in %s", logPath)
	rd, err := os.Open(logPath)
	if os.IsNotExist(err) {
		// Log file not there (yet), we allow this
		w.WriteHeader(http.StatusOK)
	} else if err != nil {
		s.log.Errorf("Failed to open log file '%s': %#v", logPath, err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(err.Error()))
	} else {
		// Log open
		defer rd.Close()
		w.WriteHeader(http.StatusOK)
		io.Copy(w, rd)
	}
}

// versionHandler returns a JSON object containing the current version & build number.
func (s *httpServer) versionHandler(w http.ResponseWriter, r *http.Request) {
	data, err := json.Marshal(s.versionInfo)
	if err != nil {
		s.log.Errorf("Failed to marshal version response: %#v", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(err.Error()))
	} else {
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	}
}

// shutdownHandler initiates a shutdown of this process and all servers started by it.
func (s *httpServer) shutdownHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if r.FormValue("mode") == "goodbye" {
		// Inform the master we're leaving for good
		if err := s.context.sendMasterLeaveCluster(); err != nil {
			s.log.Errorf("Failed to send master goodbye: %#v", err)
			handleError(w, err)
			return
		}
	}

	// Stop my services
	s.context.Stop()
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// cbMasterChanged is a callback called by the agency when the master URL is modified.
func (s *httpServer) cbMasterChanged(w http.ResponseWriter, r *http.Request) {
	s.log.Debugf("Master changed callback from %s", r.RemoteAddr)
	if r.Method != "POST" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// Interrupt runtime cluster manager
	s.context.MasterChangedCallback()
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func handleError(w http.ResponseWriter, err error) {
	if loc, ok := IsRedirect(err); ok {
		header := w.Header()
		header.Add("Location", loc)
		w.WriteHeader(http.StatusTemporaryRedirect)
	} else if client.IsBadRequest(err) {
		writeError(w, http.StatusBadRequest, err.Error())
	} else if client.IsPreconditionFailed(err) {
		writeError(w, http.StatusPreconditionFailed, err.Error())
	} else if client.IsServiceUnavailable(err) {
		writeError(w, http.StatusServiceUnavailable, err.Error())
	} else if st, ok := client.IsStatusError(err); ok {
		writeError(w, st, err.Error())
	} else {
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	if message == "" {
		message = "Unknown error"
	}
	resp := client.ErrorResponse{Error: message}
	b, _ := json.Marshal(resp)
	w.Header().Set("Content-Type", contentTypeJSON)
	w.WriteHeader(status)
	w.Write(b)
}
