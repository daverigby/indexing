// Copyright (c) 2014 Couchbase, Inc.
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
// except in compliance with the License. You may obtain a copy of the License at
//   http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software distributed under the
// License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing permissions
// and limitations under the License.
package manager

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/couchbase/cbauth"
	"github.com/couchbase/indexing/secondary/common"
	"github.com/couchbase/indexing/secondary/common/collections"
	"github.com/couchbase/indexing/secondary/logging"
	"github.com/couchbase/indexing/secondary/manager/client"
	mc "github.com/couchbase/indexing/secondary/manager/common"
	"github.com/couchbase/indexing/secondary/planner"
	"github.com/couchbase/indexing/secondary/security"
)

///////////////////////////////////////////////////////
// Type Definition
///////////////////////////////////////////////////////

//
// Index create / drop
//

type RequestType string

const (
	CREATE RequestType = "create"
	DROP   RequestType = "drop"
	BUILD  RequestType = "build"
)

type IndexRequest struct {
	Version  uint64                 `json:"version,omitempty"`
	Type     RequestType            `json:"type,omitempty"`
	Index    common.IndexDefn       `json:"index,omitempty"`
	IndexIds client.IndexIdList     `json:indexIds,omitempty"`
	Plan     map[string]interface{} `json:plan,omitempty"`
}

type IndexResponse struct {
	Version uint64 `json:"version,omitempty"`
	Code    string `json:"code,omitempty"`
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
}

//
// Index Backup / Restore
//

type LocalIndexMetadata struct {
	IndexerId        string             `json:"indexerId,omitempty"`
	NodeUUID         string             `json:"nodeUUID,omitempty"`
	StorageMode      string             `json:"storageMode,omitempty"`
	Timestamp        int64              `json:"timestamp,omitempty"`
	LocalSettings    map[string]string  `json:"localSettings,omitempty"`
	IndexTopologies  []IndexTopology    `json:"topologies,omitempty"`
	IndexDefinitions []common.IndexDefn `json:"definitions,omitempty"`
}

type ClusterIndexMetadata struct {
	Metadata    []LocalIndexMetadata                           `json:"metadata,omitempty"`
	SchedTokens map[common.IndexDefnId]*mc.ScheduleCreateToken `json:"schedTokens,omitempty"`
}

type BackupResponse struct {
	Version uint64               `json:"version,omitempty"`
	Code    string               `json:"code,omitempty"`
	Error   string               `json:"error,omitempty"`
	Result  ClusterIndexMetadata `json:"result,omitempty"`
}

type RestoreResponse struct {
	Version uint64 `json:"version,omitempty"`
	Code    string `json:"code,omitempty"`
	Error   string `json:"error,omitempty"`
}

//
// Index Status
//

type IndexStatusResponse struct {
	Version     uint64        `json:"version,omitempty"`
	Code        string        `json:"code,omitempty"`
	Error       string        `json:"error,omitempty"`
	FailedNodes []string      `json:"failedNodes,omitempty"`
	Status      []IndexStatus `json:"status,omitempty"`
}

type IndexStatus struct {
	DefnId       common.IndexDefnId `json:"defnId,omitempty"`
	InstId       common.IndexInstId `json:"instId,omitempty"`
	Name         string             `json:"name,omitempty"`
	Bucket       string             `json:"bucket,omitempty"`
	Scope        string             `json:"scope,omitempty"`
	Collection   string             `json:"collection,omitempty"`
	IsPrimary    bool               `json:"isPrimary,omitempty"`
	SecExprs     []string           `json:"secExprs,omitempty"`
	WhereExpr    string             `json:"where,omitempty"`
	IndexType    string             `json:"indexType,omitempty"`
	Status       string             `json:"status,omitempty"`
	Definition   string             `json:"definition"`
	Hosts        []string           `json:"hosts,omitempty"`
	Error        string             `json:"error,omitempty"`
	Completion   int                `json:"completion"`
	Progress     float64            `json:"progress"`
	Scheduled    bool               `json:"scheduled"`
	Partitioned  bool               `json:"partitioned"`
	NumPartition int                `json:"numPartition"`

	// PartitionMap is a map from node host:port to partitionIds,
	// telling which partition(s) are on which node(s). If an
	// index is not partitioned, it will have a single
	// partition with ID 0.
	PartitionMap map[string][]int   `json:"partitionMap"`

	NodeUUID     string             `json:"nodeUUID,omitempty"`
	NumReplica   int                `json:"numReplica"`
	IndexName    string             `json:"indexName"`
	ReplicaId    int                `json:"replicaId"`
	Stale        bool               `json:"stale"`
	LastScanTime string             `json:"lastScanTime,omitempty"`
}

type indexStatusSorter []IndexStatus

type permissionsCache struct {
	permissions map[string]bool
}

//
// Response
//

const (
	RESP_SUCCESS string = "success"
	RESP_ERROR   string = "error"
)

const (
	INDEXER_LEVEL    string = "indexer"
	BUCKET_LEVEL     string = "bucket"
	SCOPE_LEVEL      string = "scope"
	COLLECTION_LEVEL string = "collection"
	INDEX_LEVEL      string = "index"
)

type target struct {
	bucket     string
	scope      string
	collection string
	index      string
	level      string
}

//
// Internal data structure
//

type requestHandlerContext struct {
	initializer sync.Once
	finalizer   sync.Once
	mgr         *IndexManager
	clusterUrl  string

	metaDir    string
	statsDir   string
	metaCh     chan map[string]*LocalIndexMetadata
	statsCh    chan map[string]*common.Statistics
	metaCache  map[string]*LocalIndexMetadata
	statsCache map[string]*common.Statistics

	mutex  sync.RWMutex
	doneCh chan bool

	schedTokenMon *schedTokenMonitor
}

var handlerContext requestHandlerContext

///////////////////////////////////////////////////////
// Registration
///////////////////////////////////////////////////////

func registerRequestHandler(mgr *IndexManager, clusterUrl string, mux *http.ServeMux, config common.Config) {

	handlerContext.initializer.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				logging.Warnf("error encountered when registering http createIndex handler : %v.  Ignored.\n", r)
			}
		}()

		mux.HandleFunc("/createIndex", handlerContext.createIndexRequest)
		mux.HandleFunc("/createIndexRebalance", handlerContext.createIndexRequestRebalance)
		mux.HandleFunc("/dropIndex", handlerContext.dropIndexRequest)
		mux.HandleFunc("/buildIndex", handlerContext.buildIndexRequest)
		mux.HandleFunc("/getLocalIndexMetadata", handlerContext.handleLocalIndexMetadataRequest)
		mux.HandleFunc("/getIndexMetadata", handlerContext.handleIndexMetadataRequest)
		mux.HandleFunc("/restoreIndexMetadata", handlerContext.handleRestoreIndexMetadataRequest)
		mux.HandleFunc("/getIndexStatus", handlerContext.handleIndexStatusRequest)
		mux.HandleFunc("/getIndexStatement", handlerContext.handleIndexStatementRequest)
		mux.HandleFunc("/planIndex", handlerContext.handleIndexPlanRequest)
		mux.HandleFunc("/settings/storageMode", handlerContext.handleIndexStorageModeRequest)
		mux.HandleFunc("/settings/planner", handlerContext.handlePlannerRequest)
		mux.HandleFunc("/listReplicaCount", handlerContext.handleListLocalReplicaCountRequest)
		mux.HandleFunc("/getCachedLocalIndexMetadata", handlerContext.handleCachedLocalIndexMetadataRequest)
		mux.HandleFunc("/getCachedStats", handlerContext.handleCachedStats)
		mux.HandleFunc("/postScheduleCreateRequest", handlerContext.handleScheduleCreateRequest)

		cacheDir := path.Join(config["storage_dir"].String(), "cache")
		handlerContext.metaDir = path.Join(cacheDir, "meta")
		handlerContext.statsDir = path.Join(cacheDir, "stats")

		os.MkdirAll(handlerContext.metaDir, 0755)
		os.MkdirAll(handlerContext.statsDir, 0755)

		handlerContext.metaCh = make(chan map[string]*LocalIndexMetadata, 100)
		handlerContext.statsCh = make(chan map[string]*common.Statistics, 100)
		handlerContext.doneCh = make(chan bool)

		handlerContext.metaCache = make(map[string]*LocalIndexMetadata)
		handlerContext.statsCache = make(map[string]*common.Statistics)

		handlerContext.schedTokenMon = newSchedTokenMonitor(mgr)

		go handlerContext.runPersistor()
	})

	handlerContext.mgr = mgr
	handlerContext.clusterUrl = clusterUrl
}

func (m *requestHandlerContext) Close() {
	m.finalizer.Do(func() {
		close(m.doneCh)
		m.schedTokenMon.Close()
	})

}

///////////////////////////////////////////////////////
// Create / Drop Index
///////////////////////////////////////////////////////

func (m *requestHandlerContext) createIndexRequest(w http.ResponseWriter, r *http.Request) {

	m.doCreateIndex(w, r, false)

}

func (m *requestHandlerContext) createIndexRequestRebalance(w http.ResponseWriter, r *http.Request) {

	m.doCreateIndex(w, r, true)

}

func (m *requestHandlerContext) doCreateIndex(w http.ResponseWriter, r *http.Request, isRebalReq bool) {

	creds, ok := doAuth(r, w)
	if !ok {
		return
	}

	// convert request
	request := m.convertIndexRequest(r)
	if request == nil {
		sendIndexResponseWithError(http.StatusBadRequest, w, "Unable to convert request for create index")
		return
	}

	permission := fmt.Sprintf("cluster.collection[%s:%s:%s].n1ql.index!create", request.Index.Bucket, request.Index.Scope, request.Index.Collection)
	if !isAllowed(creds, []string{permission}, w) {
		return
	}

	indexDefn := request.Index

	if indexDefn.DefnId == 0 {
		defnId, err := common.NewIndexDefnId()
		if err != nil {
			sendIndexResponseWithError(http.StatusInternalServerError, w, fmt.Sprintf("Fail to generate index definition id %v", err))
			return
		}
		indexDefn.DefnId = defnId
	}

	if len(indexDefn.Using) != 0 && strings.ToLower(string(indexDefn.Using)) != "gsi" {
		if common.IndexTypeToStorageMode(indexDefn.Using) != common.GetStorageMode() {
			sendIndexResponseWithError(http.StatusInternalServerError, w, fmt.Sprintf("Storage Mode Mismatch %v", indexDefn.Using))
			return
		}
	}

	// call the index manager to handle the DDL
	logging.Debugf("RequestHandler::createIndexRequest: invoke IndexManager for create index bucket %s name %s",
		indexDefn.Bucket, indexDefn.Name)

	if err := m.mgr.HandleCreateIndexDDL(&indexDefn, isRebalReq); err == nil {
		// No error, return success
		sendIndexResponse(w)
	} else {
		// report failure
		sendIndexResponseWithError(http.StatusInternalServerError, w, fmt.Sprintf("%v", err))
	}

}

func (m *requestHandlerContext) dropIndexRequest(w http.ResponseWriter, r *http.Request) {

	creds, ok := doAuth(r, w)
	if !ok {
		return
	}

	// convert request
	request := m.convertIndexRequest(r)
	if request == nil {
		sendIndexResponseWithError(http.StatusBadRequest, w, "Unable to convert request for drop index")
		return
	}

	permission := fmt.Sprintf("cluster.collection[%s:%s:%s].n1ql.index!drop", request.Index.Bucket, request.Index.Scope, request.Index.Collection)
	if !isAllowed(creds, []string{permission}, w) {
		return
	}

	// call the index manager to handle the DDL
	indexDefn := request.Index

	if indexDefn.RealInstId == 0 {
		if err := m.mgr.HandleDeleteIndexDDL(indexDefn.DefnId); err == nil {
			// No error, return success
			sendIndexResponse(w)
		} else {
			// report failure
			sendIndexResponseWithError(http.StatusInternalServerError, w, fmt.Sprintf("%v", err))
		}
	} else if indexDefn.InstId != 0 {
		if err := m.mgr.DropOrPruneInstance(indexDefn, true); err == nil {
			// No error, return success
			sendIndexResponse(w)
		} else {
			// report failure
			sendIndexResponseWithError(http.StatusInternalServerError, w, fmt.Sprintf("%v", err))
		}
	} else {
		// report failure
		sendIndexResponseWithError(http.StatusInternalServerError, w, fmt.Sprintf("Missing index inst id for defn %v", indexDefn.DefnId))
	}
}

func (m *requestHandlerContext) buildIndexRequest(w http.ResponseWriter, r *http.Request) {

	creds, ok := doAuth(r, w)
	if !ok {
		return
	}

	// convert request
	request := m.convertIndexRequest(r)
	if request == nil {
		sendIndexResponseWithError(http.StatusBadRequest, w, "Unable to convert request for build index")
		return
	}

	permission := fmt.Sprintf("cluster.collection[%s:%s:%s].n1ql.index!build", request.Index.Bucket, request.Index.Scope, request.Index.Collection)
	if !isAllowed(creds, []string{permission}, w) {
		return
	}

	// call the index manager to handle the DDL
	indexIds := request.IndexIds
	if err := m.mgr.HandleBuildIndexDDL(indexIds); err == nil {
		// No error, return success
		sendIndexResponse(w)
	} else {
		// report failure
		sendIndexResponseWithError(http.StatusInternalServerError, w, fmt.Sprintf("%v", err))
	}
}

func (m *requestHandlerContext) convertIndexRequest(r *http.Request) *IndexRequest {

	req := &IndexRequest{}

	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(r.Body); err != nil {
		logging.Debugf("RequestHandler::convertIndexRequest: unable to read request body, err %v", err)
		return nil
	}

	if err := json.Unmarshal(buf.Bytes(), req); err != nil {
		logging.Debugf("RequestHandler::convertIndexRequest: unable to unmarshall request body. Buf = %s, err %v", logging.TagStrUD(buf), err)
		return nil
	}

	// Set default scope and collection name if incoming request dont have them
	req.Index.SetCollectionDefaults()

	return req
}

//////////////////////////////////////////////////////
// Index Status
///////////////////////////////////////////////////////

func (m *requestHandlerContext) handleIndexStatusRequest(w http.ResponseWriter, r *http.Request) {

	creds, ok := doAuth(r, w)
	if !ok {
		return
	}

	bucket := m.getBucket(r)
	scope := m.getScope(r)
	collection := m.getCollection(r)
	index := m.getIndex(r)

	t, err := validateRequest(bucket, scope, collection, index)
	if err != nil {
		logging.Debugf("RequestHandler::handleIndexStatusRequest: Error %v", err)
		resp := &IndexStatusResponse{Code: RESP_ERROR, Error: err.Error()}
		send(http.StatusInternalServerError, w, resp)
		return
	}

	getAll := false
	val := r.FormValue("getAll")
	if len(val) != 0 && val == "true" {
		getAll = true
	}

	list, failedNodes, err := m.getIndexStatus(creds, t, getAll)
	if err == nil && len(failedNodes) == 0 {
		sort.Sort(indexStatusSorter(list))
		resp := &IndexStatusResponse{Code: RESP_SUCCESS, Status: list}
		send(http.StatusOK, w, resp)
	} else {
		logging.Debugf("RequestHandler::handleIndexStatusRequest: failed nodes %v", failedNodes)
		sort.Sort(indexStatusSorter(list))
		resp := &IndexStatusResponse{Code: RESP_ERROR, Error: "Fail to retrieve cluster-wide metadata from index service",
			Status: list, FailedNodes: failedNodes}
		send(http.StatusInternalServerError, w, resp)
	}
}

func (m *requestHandlerContext) getBucket(r *http.Request) string {

	return r.FormValue("bucket")
}

func (m *requestHandlerContext) getScope(r *http.Request) string {

	return r.FormValue("scope")
}

func (m *requestHandlerContext) getCollection(r *http.Request) string {

	return r.FormValue("collection")
}

func (m *requestHandlerContext) getIndex(r *http.Request) string {

	return r.FormValue("index")
}

func (m *requestHandlerContext) getIndexStatus(creds cbauth.Creds, t *target, getAll bool) ([]IndexStatus, []string, error) {

	var cinfo *common.ClusterInfoCache
	cinfo = m.mgr.reqcic.GetClusterInfoCache()

	if cinfo == nil {
		return nil, nil, errors.New("ClusterInfoCache unavailable in IndexManager")
	}

	cinfo.RLock()
	defer cinfo.RUnlock()

	// find all nodes that has a index http service
	nids := cinfo.GetNodesByServiceType(common.INDEX_HTTP_SERVICE)

	numReplicas := make(map[common.IndexDefnId]common.Counter)
	defns := make(map[common.IndexDefnId]common.IndexDefn)
	list := make([]IndexStatus, 0)
	failedNodes := make([]string, 0)
	metaToCache := make(map[string]*LocalIndexMetadata)
	statsToCache := make(map[string]*common.Statistics)

	defnToHostMap := make(map[common.IndexDefnId][]string)
	isInstanceDeferred := make(map[common.IndexInstId]bool)
	permissionCache := initPermissionsCache()

	mergeCounter := func(defnId common.IndexDefnId, counter common.Counter) {
		if current, ok := numReplicas[defnId]; ok {
			newValue, merged, err := current.MergeWith(counter)
			if err != nil {
				logging.Errorf("Fail to merge replica count. Error: %v", err)
				return
			}

			if merged {
				numReplicas[defnId] = newValue
			}

			return
		}

		if counter.IsValid() {
			numReplicas[defnId] = counter
		}
	}

	addHost := func(defnId common.IndexDefnId, hostAddr string) {
		if hostList, ok := defnToHostMap[defnId]; ok {
			for _, host := range hostList {
				if strings.Compare(hostAddr, host) == 0 {
					return
				}
			}
		}
		defnToHostMap[defnId] = append(defnToHostMap[defnId], hostAddr)
	}

	buildTopologyMapPerCollection := func(topologies []IndexTopology) map[string]map[string]map[string]*IndexTopology {
		topoMap := make(map[string]map[string]map[string]*IndexTopology)
		for i, _ := range topologies {
			t := &topologies[i]
			t.SetCollectionDefaults()
			if _, ok := topoMap[t.Bucket]; !ok {
				topoMap[t.Bucket] = make(map[string]map[string]*IndexTopology)
			}
			if _, ok := topoMap[t.Bucket][t.Scope]; !ok {
				topoMap[t.Bucket][t.Scope] = make(map[string]*IndexTopology)
			}
			topoMap[t.Bucket][t.Scope][t.Collection] = t
		}
		return topoMap
	}

	for _, nid := range nids {

		mgmtAddr, err := cinfo.GetServiceAddress(nid, "mgmt")
		if err != nil {
			logging.Errorf("RequestHandler::getIndexStatus: Error from GetServiceAddress (mgmt) for node id %v. Error = %v", nid, err)
			continue
		}

		addr, err := cinfo.GetServiceAddress(nid, common.INDEX_HTTP_SERVICE)
		if err == nil {

			u, err := security.GetURL(addr)
			if err != nil {
				logging.Debugf("RequestHandler::getIndexStatus: Fail to parse URL %v", addr)
				failedNodes = append(failedNodes, mgmtAddr)
				continue
			}

			stale := false
			metaToCache[u.Host] = nil
			// TODO: It is not required to fetch metadata for entire node when target is for a specific
			// bucket or collection
			localMeta, latest, err := m.getLocalMetadataForNode(addr, u.Host, cinfo)
			if localMeta == nil || err != nil {
				logging.Debugf("RequestHandler::getIndexStatus: Error while retrieving %v with auth %v", addr+"/getLocalIndexMetadata", err)
				failedNodes = append(failedNodes, mgmtAddr)
				continue
			}

			topoMap := buildTopologyMapPerCollection(localMeta.IndexTopologies)
			if !latest {
				stale = true
			} else {
				metaToCache[u.Host] = localMeta
			}

			statsToCache[u.Host] = nil
			stats, latest, err := m.getStatsForNode(addr, u.Host, cinfo)
			if stats == nil || err != nil {
				logging.Debugf("RequestHandler::getIndexStatus: Error while retrieving %v with auth %v", addr+"/stats?async=true", err)
				failedNodes = append(failedNodes, mgmtAddr)
				continue
			}

			if !latest {
				stale = true
			} else {
				statsToCache[u.Host] = stats
			}

			for _, defn := range localMeta.IndexDefinitions {
				defn.SetCollectionDefaults()

				if !shouldProcess(t, defn.Bucket, defn.Scope, defn.Collection, defn.Name) {
					continue
				}

				accessAllowed := permissionCache.isAllowed(creds, defn.Bucket, defn.Scope, defn.Collection, "list")
				if !accessAllowed {
					continue
				}

				mergeCounter(defn.DefnId, defn.NumReplica2)

				if topology, ok := topoMap[defn.Bucket][defn.Scope][defn.Collection]; ok && topology != nil {

					instances := topology.GetIndexInstancesByDefn(defn.DefnId)
					for _, instance := range instances {

						state, errStr := topology.GetStatusByInst(defn.DefnId, common.IndexInstId(instance.InstId))

						if state != common.INDEX_STATE_CREATED &&
							state != common.INDEX_STATE_DELETED &&
							state != common.INDEX_STATE_NIL {

							stateStr := "Not Available"
							switch state {
							case common.INDEX_STATE_READY:
								stateStr = "Created"
							case common.INDEX_STATE_INITIAL:
								stateStr = "Building"
							case common.INDEX_STATE_CATCHUP:
								stateStr = "Building"
							case common.INDEX_STATE_ACTIVE:
								stateStr = "Ready"
							}

							if instance.RState == uint32(common.REBAL_PENDING) && state != common.INDEX_STATE_READY {
								stateStr = "Replicating"
							}

							if state == common.INDEX_STATE_INITIAL || state == common.INDEX_STATE_CATCHUP {
								if len(instance.OldStorageMode) != 0 {

									if instance.OldStorageMode == common.ForestDB && instance.StorageMode == common.PlasmaDB {
										stateStr = "Building (Upgrading)"
									}

									if instance.StorageMode == common.ForestDB && instance.OldStorageMode == common.PlasmaDB {
										stateStr = "Building (Downgrading)"
									}
								}
							}

							if state == common.INDEX_STATE_READY {
								if len(instance.OldStorageMode) != 0 {

									if instance.OldStorageMode == common.ForestDB && instance.StorageMode == common.PlasmaDB {
										stateStr = "Created (Upgrading)"
									}

									if instance.StorageMode == common.ForestDB && instance.OldStorageMode == common.PlasmaDB {
										stateStr = "Created (Downgrading)"
									}
								}
							}

							if indexerState, ok := stats.ToMap()["indexer_state"]; ok {
								if indexerState == "Paused" {
									stateStr = "Paused"
								} else if indexerState == "Bootstrap" || indexerState == "Warmup" {
									stateStr = "Warmup"
								}
							}

							if len(errStr) != 0 {
								stateStr = "Error"
							}

							name := common.FormatIndexInstDisplayName(defn.Name, int(instance.ReplicaId))
							prefix := common.GetStatsPrefix(defn.Bucket, defn.Scope, defn.Collection,
								defn.Name, int(instance.ReplicaId), 0, false)

							completion := int(0)
							key := common.GetIndexStatKey(prefix, "build_progress")
							if progress, ok := stats.ToMap()[key]; ok {
								completion = int(progress.(float64))
							}

							progress := float64(0)
							key = fmt.Sprintf("%v:completion_progress", instance.InstId)
							if stat, ok := stats.ToMap()[key]; ok {
								progress = math.Float64frombits(uint64(stat.(float64)))
							}

							lastScanTime := "NA"
							key = common.GetIndexStatKey(prefix, "last_known_scan_time")
							if scanTime, ok := stats.ToMap()[key]; ok {
								nsecs := int64(scanTime.(float64))
								if nsecs != 0 {
									lastScanTime = time.Unix(0, nsecs).Format(time.UnixDate)
								}
							}

							partitionMap := make(map[string][]int)
							for _, partnDef := range instance.Partitions {
								partitionMap[mgmtAddr] = append(partitionMap[mgmtAddr], int(partnDef.PartId))
							}

							addHost(defn.DefnId, mgmtAddr)
							isInstanceDeferred[common.IndexInstId(instance.InstId)] = defn.Deferred
							defn.NumPartitions = instance.NumPartitions

							status := IndexStatus{
								DefnId:       defn.DefnId,
								InstId:       common.IndexInstId(instance.InstId),
								Name:         name,
								Bucket:       defn.Bucket,
								Scope:        defn.Scope,
								Collection:   defn.Collection,
								IsPrimary:    defn.IsPrimary,
								SecExprs:     defn.SecExprs,
								WhereExpr:    defn.WhereExpr,
								IndexType:    string(defn.Using),
								Status:       stateStr,
								Error:        errStr,
								Hosts:        []string{mgmtAddr},
								Definition:   common.IndexStatement(defn, int(instance.NumPartitions), -1, true),
								Completion:   completion,
								Progress:     progress,
								Scheduled:    instance.Scheduled,
								Partitioned:  common.IsPartitioned(defn.PartitionScheme),
								NumPartition: len(instance.Partitions),
								PartitionMap: partitionMap,
								NodeUUID:     localMeta.NodeUUID,
								NumReplica:   int(defn.GetNumReplica()),
								IndexName:    defn.Name,
								ReplicaId:    int(instance.ReplicaId),
								Stale:        stale,
								LastScanTime: lastScanTime,
							}

							list = append(list, status)
						}
					}
				}
				defns[defn.DefnId] = defn
			}
		} else {
			logging.Debugf("RequestHandler::getIndexStatus: Error from GetServiceAddress (indexHttp) for node id %v. Error = %v", nid, err)
			failedNodes = append(failedNodes, mgmtAddr)
			continue
		}
	}

	//Fix replica count
	for i, index := range list {
		if counter, ok := numReplicas[index.DefnId]; ok {
			numReplica, exist := counter.Value()
			if exist {
				list[i].NumReplica = int(numReplica)
			}
		}
	}

	// Fix index definition so that the "nodes" field inside
	// "with" clause show the current set of nodes on which
	// the index resides.
	//
	// If the index resides on different nodes, the "nodes" clause
	// is populated on UI irrespective of whether the index is
	// explicitly defined with "nodes" clause or not
	//
	// If the index resides only on one node, the "nodes" clause is
	// populated on UI only if the index definition is explicitly
	// defined with "nodes" clause
	for i, index := range list {
		defnId := index.DefnId
		defn := defns[defnId]
		if len(defnToHostMap[defnId]) > 1 || defn.Nodes != nil {
			defn.Nodes = defnToHostMap[defnId]
			// The deferred field will be set to true by default for a rebalanced index
			// For the non-rebalanced index, it can either be true or false depending on
			// how it was created
			defn.Deferred = isInstanceDeferred[index.InstId]
			list[i].Definition = common.IndexStatement(defn, int(defn.NumPartitions), index.NumReplica, true)
		}
	}

	if !getAll {
		list = m.consolideIndexStatus(list)
	}

	schedIndexes := m.schedTokenMon.getIndexes()
	schedIndexList := make([]IndexStatus, 0, len(schedIndexes))
	for _, idx := range schedIndexes {
		if _, ok := defns[idx.DefnId]; ok {
			continue
		}

		schedIndexList = append(schedIndexList, *idx)
	}

	list = append(list, schedIndexList...)

	// persist local meta and stats to disk cache
	m.metaCh <- metaToCache
	m.statsCh <- statsToCache

	return list, failedNodes, nil
}

func (m *requestHandlerContext) consolideIndexStatus(statuses []IndexStatus) []IndexStatus {

	statusMap := make(map[common.IndexInstId]IndexStatus)

	for _, status := range statuses {
		if s2, ok := statusMap[status.InstId]; !ok {
			status.NodeUUID = ""
			statusMap[status.InstId] = status
		} else {
			s2.Status = m.consolideStateStr(s2.Status, status.Status)
			s2.Hosts = append(s2.Hosts, status.Hosts...)
			s2.Completion = (s2.Completion + status.Completion) / 2
			s2.Progress = (s2.Progress + status.Progress) / 2.0
			s2.NumPartition += status.NumPartition
			s2.NodeUUID = ""
			if len(status.Error) != 0 {
				s2.Error = fmt.Sprintf("%v %v", s2.Error, status.Error)
			}

			for host, partitions := range status.PartitionMap {
				s2.PartitionMap[host] = partitions
			}
			s2.Stale = s2.Stale || status.Stale

			statusMap[status.InstId] = s2
		}
	}

	result := make([]IndexStatus, 0, len(statuses))
	for _, status := range statusMap {
		result = append(result, status)
	}

	return result
}

func (m *requestHandlerContext) consolideStateStr(str1 string, str2 string) string {

	if str1 == "Paused" || str2 == "Paused" {
		return "Paused"
	}

	if str1 == "Warmup" || str2 == "Warmup" {
		return "Warmup"
	}

	if strings.HasPrefix(str1, "Created") || strings.HasPrefix(str2, "Created") {
		if str1 == str2 {
			return str1
		}
		return "Created"
	}

	if strings.HasPrefix(str1, "Building") || strings.HasPrefix(str2, "Building") {
		if str1 == str2 {
			return str1
		}
		return "Building"
	}

	if str1 == "Replicating" || str2 == "Replicating" {
		return "Replicating"
	}

	// must be ready
	return str1
}

//////////////////////////////////////////////////////
// Index Statement
///////////////////////////////////////////////////////

func (m *requestHandlerContext) handleIndexStatementRequest(w http.ResponseWriter, r *http.Request) {

	creds, ok := doAuth(r, w)
	if !ok {
		return
	}

	bucket := m.getBucket(r)
	scope := m.getScope(r)
	collection := m.getCollection(r)
	index := m.getIndex(r)

	t, err := validateRequest(bucket, scope, collection, index)
	if err != nil {
		logging.Debugf("RequestHandler::handleIndexMetadataRequest: err %v", err)
		resp := &BackupResponse{Code: RESP_ERROR, Error: err.Error()}
		send(http.StatusInternalServerError, w, resp)
		return
	}

	list, err := m.getIndexStatement(creds, t)
	if err == nil {
		sort.Strings(list)
		send(http.StatusOK, w, list)
	} else {
		send(http.StatusInternalServerError, w, err.Error())
	}
}

func (m *requestHandlerContext) getIndexStatement(creds cbauth.Creds, t *target) ([]string, error) {

	indexes, failedNodes, err := m.getIndexStatus(creds, t, false)
	if err != nil {
		return nil, err
	}
	if len(failedNodes) != 0 {
		return nil, errors.New(fmt.Sprintf("Failed to connect to indexer nodes %v", failedNodes))
	}

	defnMap := make(map[common.IndexDefnId]bool)
	statements := ([]string)(nil)
	for _, index := range indexes {
		if _, ok := defnMap[index.DefnId]; !ok {
			defnMap[index.DefnId] = true
			statements = append(statements, index.Definition)
		}
	}

	return statements, nil
}

///////////////////////////////////////////////////////
// ClusterIndexMetadata
///////////////////////////////////////////////////////

func (m *requestHandlerContext) handleIndexMetadataRequest(w http.ResponseWriter, r *http.Request) {

	creds, ok := doAuth(r, w)
	if !ok {
		return
	}

	bucket := m.getBucket(r)
	scope := m.getScope(r)
	collection := m.getCollection(r)

	index := m.getIndex(r)
	if len(index) != 0 {
		err := errors.New("RequestHandler::handleIndexMetadataRequest, err: Index level metadata requests are not supported")
		resp := &BackupResponse{Code: RESP_ERROR, Error: err.Error()}
		send(http.StatusInternalServerError, w, resp)
		return
	}

	t, err := validateRequest(bucket, scope, collection, index)
	if err != nil {
		logging.Debugf("RequestHandler::handleIndexMetadataRequest: err %v", err)
		resp := &BackupResponse{Code: RESP_ERROR, Error: err.Error()}
		send(http.StatusInternalServerError, w, resp)
		return
	}

	meta, err := m.getIndexMetadata(creds, t)
	if err == nil {
		resp := &BackupResponse{Code: RESP_SUCCESS, Result: *meta}
		send(http.StatusOK, w, resp)
	} else {
		logging.Debugf("RequestHandler::handleIndexMetadataRequest: err %v", err)
		resp := &BackupResponse{Code: RESP_ERROR, Error: err.Error()}
		send(http.StatusInternalServerError, w, resp)
	}
}

func (m *requestHandlerContext) getIndexMetadata(creds cbauth.Creds, t *target) (*ClusterIndexMetadata, error) {

	cinfo, err := m.mgr.FetchNewClusterInfoCache()
	if err != nil {
		return nil, err
	}

	permissionsCache := initPermissionsCache()

	// find all nodes that has a index http service
	nids := cinfo.GetNodesByServiceType(common.INDEX_HTTP_SERVICE)

	clusterMeta := &ClusterIndexMetadata{Metadata: make([]LocalIndexMetadata, len(nids))}

	for i, nid := range nids {

		addr, err := cinfo.GetServiceAddress(nid, common.INDEX_HTTP_SERVICE)
		if err == nil {

			url := "/getLocalIndexMetadata"
			if len(t.bucket) != 0 {
				url += "?bucket=" + t.bucket
			}
			if len(t.scope) != 0 {
				url += "&scope=" + t.scope
			}
			if len(t.collection) != 0 {
				url += "&collection=" + t.collection
			}
			if len(t.index) != 0 {
				url += "&index=" + t.index
			}

			resp, err := getWithAuth(addr + url)
			if err != nil {
				logging.Debugf("RequestHandler::getIndexMetadata: Error while retrieving %v with auth %v", addr+"/getLocalIndexMetadata", err)
				return nil, errors.New(fmt.Sprintf("Fail to retrieve index definition from url %s", addr))
			}
			defer resp.Body.Close()

			localMeta := new(LocalIndexMetadata)
			status := convertResponse(resp, localMeta)
			if status == RESP_ERROR {
				return nil, errors.New(fmt.Sprintf("Fail to retrieve local metadata from url %s.", addr))
			}

			newLocalMeta := LocalIndexMetadata{
				IndexerId:   localMeta.IndexerId,
				NodeUUID:    localMeta.NodeUUID,
				StorageMode: localMeta.StorageMode,
			}

			for _, topology := range localMeta.IndexTopologies {
				if permissionsCache.isAllowed(creds, topology.Bucket, topology.Scope, topology.Collection, "list") {
					newLocalMeta.IndexTopologies = append(newLocalMeta.IndexTopologies, topology)
				}
			}

			for _, defn := range localMeta.IndexDefinitions {
				if permissionsCache.isAllowed(creds, defn.Bucket, defn.Scope, defn.Collection, "list") {
					newLocalMeta.IndexDefinitions = append(newLocalMeta.IndexDefinitions, defn)
				}
			}

			clusterMeta.Metadata[i] = newLocalMeta

		} else {
			return nil, errors.New(fmt.Sprintf("Fail to retrieve http endpoint for index node"))
		}
	}

	return clusterMeta, nil
}

func (m *requestHandlerContext) convertIndexMetadataRequest(r *http.Request) *ClusterIndexMetadata {
	var check map[string]interface{}

	meta := &ClusterIndexMetadata{}

	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(r.Body); err != nil {
		logging.Debugf("RequestHandler::convertIndexRequest: unable to read request body, err %v", err)
		return nil
	}

	logging.Debugf("requestHandler.convertIndexMetadataRequest(): input %v", string(buf.Bytes()))

	if err := json.Unmarshal(buf.Bytes(), &check); err != nil {
		logging.Debugf("RequestHandler::convertIndexMetadataRequest: unable to unmarshall request body. Buf = %s, err %v", buf, err)
		return nil
	} else if _, ok := check["metadata"]; !ok {
		logging.Debugf("RequestHandler::convertIndexMetadataRequest: invalid shape of request body. Buf = %s, err %v", buf, err)
		return nil
	}

	if err := json.Unmarshal(buf.Bytes(), meta); err != nil {
		logging.Debugf("RequestHandler::convertIndexMetadataRequest: unable to unmarshall request body. Buf = %s, err %v", buf, err)
		return nil
	}

	return meta
}

func validateRequest(bucket, scope, collection, index string) (*target, error) {
	// When bucket is not specified, return indexer level stats
	if len(bucket) == 0 {
		if len(scope) == 0 && len(collection) == 0 && len(index) == 0 {
			return &target{level: INDEXER_LEVEL}, nil
		}
		return nil, errors.New("Missing bucket parameter as scope/collection/index are specified")
	} else {
		// When bucket is specified and scope, collection are empty, return results
		// from all indexes belonging to that bucket
		if len(scope) == 0 && len(collection) == 0 {
			if len(index) != 0 {
				return nil, errors.New("Missing scope and collection parameters as index parameter is specified")
			}
			return &target{bucket: bucket, level: BUCKET_LEVEL}, nil
		} else if len(scope) != 0 && len(collection) == 0 {
			if len(index) != 0 {
				return nil, errors.New("Missing collection parameter as index parameter is specified")
			}

			return &target{bucket: bucket, scope: scope, level: SCOPE_LEVEL}, nil
		} else if len(scope) == 0 && len(collection) != 0 {
			return nil, errors.New("Missing scope parameter as collection paramter is specified")
		} else { // Both collection and scope are specified
			if len(index) != 0 {
				return &target{bucket: bucket, scope: scope, collection: collection, index: index, level: INDEX_LEVEL}, nil
			}
			return &target{bucket: bucket, scope: scope, collection: collection, level: COLLECTION_LEVEL}, nil
		}
		return nil, errors.New("Missing scope or collection parameters")
	}
	return nil, nil
}

func getFilters(r *http.Request, bucket string) (map[string]bool, string, error) {
	// Validation rules:
	//
	// 1. When include or exclude filter is specified, scope and collection
	//    parameters should NOT be specified.
	// 2. When include or exclude filter is specified, bucket parameter
	//    SHOULD be specified.
	// 3. Either include or exclude should be specified. Not both.

	include := r.FormValue("include")
	exclude := r.FormValue("exclude")
	scope := r.FormValue("scope")
	collection := r.FormValue("collection")

	if len(include) != 0 || len(exclude) != 0 {
		if len(bucket) == 0 {
			return nil, "", fmt.Errorf("Malformed input: include/exclude parameters are specified without bucket.")
		}

		if len(scope) != 0 || len(collection) != 0 {
			return nil, "", fmt.Errorf("Malformed input: include/exclude parameters are specified with scope/collection.")
		}
	}

	if len(include) != 0 && len(exclude) != 0 {
		return nil, "", fmt.Errorf("Malformed input: include and exclude both parameters are specified.")
	}

	getFilter := func(s string) string {
		comp := strings.Split(s, ".")
		if len(comp) == 1 || len(comp) == 2 {
			return s
		}

		return ""
	}

	filterType := ""
	filters := make(map[string]bool)

	if len(include) != 0 {
		filterType = "include"
		incl := strings.Split(include, ",")
		for _, inc := range incl {
			filter := getFilter(inc)
			if filter == "" {
				return nil, "", fmt.Errorf("Malformed input: include filter is malformed (%v) (%v)", incl, inc)
			}

			filters[filter] = true
		}
	}

	if len(exclude) != 0 {
		filterType = "exclude"
		excl := strings.Split(exclude, ",")
		for _, exc := range excl {
			filter := getFilter(exc)
			if filter == "" {
				return nil, "", fmt.Errorf("Malformed input: exclude filter is malformed (%v) (%v)", excl, exc)
			}

			filters[filter] = true
		}
	}

	// TODO: Do we need any more validations?
	return filters, filterType, nil
}

func applyFilters(bucket, idxBucket, scope, collection, name string,
	filters map[string]bool, filterType string) bool {

	if bucket == "" {
		return true
	}

	if idxBucket != bucket {
		return false
	}

	if filterType == "" {
		return true
	}

	if _, ok := filters[scope]; ok {
		if filterType == "include" {
			return true
		} else {
			return false
		}
	}

	if _, ok := filters[fmt.Sprintf("%v.%v", scope, collection)]; ok {
		if filterType == "include" {
			return true
		} else {
			return false
		}
	}

	if name != "" {
		if _, ok := filters[fmt.Sprintf("%v.%v.%v", scope, collection, name)]; ok {
			if filterType == "include" {
				return true
			} else {
				return false
			}
		}
	}

	if filterType == "include" {
		return false
	}

	return true
}

func getRestoreRemapParam(r *http.Request) (map[string]string, error) {

	remap := make(map[string]string)

	remapStr := r.FormValue("remap")
	if remapStr == "" {
		return remap, nil
	}

	remaps := strings.Split(remapStr, ",")

	// Cache the collection level remaps for verification
	collRemap := make(map[string]string)

	for _, rm := range remaps {

		rmp := strings.Split(rm, ":")
		if len(rmp) > 2 || len(rmp) < 2 {
			return nil, fmt.Errorf("Malformed input. Missing source/target in remap %v", remapStr)
		}

		source := rmp[0]
		target := rmp[1]

		src := strings.Split(source, ".")
		tgt := strings.Split(target, ".")

		if len(src) != len(tgt) {
			return nil, fmt.Errorf("Malformed input. source and target in remap should be at same level %v", remapStr)
		}

		switch len(src) {

		case 2:
			// This is collection level remap
			// Search for overlapping scope level remap
			// Allow overlapping at the target, but not source
			if _, ok := remap[src[0]]; ok {
				return nil, fmt.Errorf("Malformed input. Overlapping remaps %v", remapStr)
			}

			remap[source] = target
			collRemap[src[0]] = src[1]

		case 1:
			// This is scope level remap.
			// Search for overlapping collection level remap
			// Allow overlapping at the target, but not source
			if _, ok := collRemap[source]; ok {
				return nil, fmt.Errorf("Malformed input. Overlapping remaps %v", remapStr)
			}

			remap[source] = target

		default:
			return nil, fmt.Errorf("Malformed input remap %v", remapStr)
		}
	}

	return remap, nil
}

///////////////////////////////////////////////////////
// LocalIndexMetadata
///////////////////////////////////////////////////////

func (m *requestHandlerContext) handleLocalIndexMetadataRequest(w http.ResponseWriter, r *http.Request) {

	creds, ok := doAuth(r, w)
	if !ok {
		return
	}

	bucket := m.getBucket(r)
	scope := m.getScope(r)
	collection := m.getCollection(r)
	index := m.getIndex(r)
	if len(index) != 0 {
		err := errors.New("RequestHandler::handleLocalIndexMetadataRequest, err: Index level metadata requests are not supported")
		resp := &BackupResponse{Code: RESP_ERROR, Error: err.Error()}
		send(http.StatusBadRequest, w, resp)
		return
	}

	t, err := validateRequest(bucket, scope, collection, index)
	if err != nil {
		logging.Debugf("RequestHandler::handleLocalIndexMetadataRequest: err %v", err)
		errStr := fmt.Sprintf(" Unable to retrieve local index metadata due to: %v", err.Error())
		sendHttpError(w, errStr, http.StatusBadRequest)
		return
	}

	var filters map[string]bool
	var filterType string
	filters, filterType, err = getFilters(r, bucket)
	if err != nil {
		logging.Infof("RequestHandler::handleLocalIndexMetadataRequest: err %v", err)
		errStr := fmt.Sprintf(" Unable to retrieve local index metadata due to: %v", err.Error())
		sendHttpError(w, errStr, http.StatusBadRequest)
		return
	}

	if len(filters) == 0 {
		if t.level == SCOPE_LEVEL {
			filterType = "include"
			filters[t.scope] = true
		} else if t.level == COLLECTION_LEVEL {
			filterType = "include"
			filters[fmt.Sprintf("%v.%v", t.scope, t.collection)] = true
		} else if t.level == INDEX_LEVEL {
			filterType = "include"
			filters[fmt.Sprintf("%v.%v.%v", t.scope, t.collection, t.index)] = true
		}
	}

	meta, err := m.getLocalIndexMetadata(creds, bucket, filters, filterType)
	if err == nil {
		send(http.StatusOK, w, meta)
	} else {
		logging.Debugf("RequestHandler::handleLocalIndexMetadataRequest: err %v", err)
		sendHttpError(w, " Unable to retrieve index metadata", http.StatusInternalServerError)
	}
}

func (m *requestHandlerContext) getLocalIndexMetadata(creds cbauth.Creds,
	bucket string, filters map[string]bool, filterType string) (meta *LocalIndexMetadata, err error) {

	repo := m.mgr.getMetadataRepo()
	permissionsCache := initPermissionsCache()

	meta = &LocalIndexMetadata{IndexTopologies: nil, IndexDefinitions: nil}
	indexerId, err := repo.GetLocalIndexerId()
	if err != nil {
		return nil, err
	}
	meta.IndexerId = string(indexerId)

	nodeUUID, err := repo.GetLocalNodeUUID()
	if err != nil {
		return nil, err
	}
	meta.NodeUUID = string(nodeUUID)

	meta.StorageMode = string(common.StorageModeToIndexType(common.GetStorageMode()))
	meta.LocalSettings = make(map[string]string)

	meta.Timestamp = time.Now().UnixNano()

	if exclude, err := m.mgr.GetLocalValue("excludeNode"); err == nil {
		meta.LocalSettings["excludeNode"] = exclude
	}

	iter, err := repo.NewIterator()
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	var defn *common.IndexDefn
	_, defn, err = iter.Next()
	for err == nil {
		if applyFilters(bucket, defn.Bucket, defn.Scope, defn.Collection, defn.Name, filters, filterType) {
			if permissionsCache.isAllowed(creds, defn.Bucket, defn.Scope, defn.Collection, "list") {
				meta.IndexDefinitions = append(meta.IndexDefinitions, *defn)
			}
		}
		_, defn, err = iter.Next()
	}

	iter1, err := repo.NewTopologyIterator()
	if err != nil {
		return nil, err
	}
	defer iter1.Close()

	var topology *IndexTopology
	topology, err = iter1.Next()
	for err == nil {
		if applyFilters(bucket, topology.Bucket, topology.Scope, topology.Collection, "", filters, filterType) {
			if permissionsCache.isAllowed(creds, topology.Bucket, topology.Scope, topology.Collection, "list") {
				meta.IndexTopologies = append(meta.IndexTopologies, *topology)
			}
		}
		topology, err = iter1.Next()
	}

	return meta, nil
}

func shouldProcess(t *target, defnBucket, defnScope, defnColl, defnName string) bool {
	if t.level == INDEXER_LEVEL {
		return true
	}
	if t.level == BUCKET_LEVEL && (t.bucket == defnBucket) {
		return true
	}
	if t.level == SCOPE_LEVEL && (t.bucket == defnBucket) && t.scope == defnScope {
		return true
	}
	if t.level == COLLECTION_LEVEL && (t.bucket == defnBucket) && t.scope == defnScope && t.collection == defnColl {
		return true
	}
	if t.level == INDEX_LEVEL && (t.bucket == defnBucket) && t.scope == defnScope && t.collection == defnColl && t.index == defnName {
		return true
	}
	return false
}

func initPermissionsCache() *permissionsCache {
	p := &permissionsCache{}
	p.permissions = make(map[string]bool)
	return p
}

func (p *permissionsCache) isAllowed(creds cbauth.Creds, bucket, scope, collection, op string) bool {

	checkAndAddBucketLevelPermission := func(bucket string) bool {
		if bucketLevelPermission, ok := p.permissions[bucket]; ok {
			return bucketLevelPermission
		} else {
			permission := fmt.Sprintf("cluster.bucket[%s].n1ql.index!%s", bucket, op)
			p.permissions[bucket] = isAllowed(creds, []string{permission}, nil)
			return p.permissions[bucket]
		}
	}

	checkAndAddScopeLevelPermission := func(bucket, scope string) bool {
		scopeLevel := fmt.Sprintf("%s:%s", bucket, scope)
		if scopeLevelPermission, ok := p.permissions[scopeLevel]; ok {
			return scopeLevelPermission
		} else {
			permission := fmt.Sprintf("cluster.scope[%s].n1ql.index!%s", scopeLevel, op)
			p.permissions[scopeLevel] = isAllowed(creds, []string{permission}, nil)
			return p.permissions[scopeLevel]
		}
	}

	checkAndAddCollectionLevelPermission := func(bucket, scope, collection string) bool {
		collectionLevel := fmt.Sprintf("%s:%s:%s", bucket, scope, collection)
		if collectionLevelPermission, ok := p.permissions[collectionLevel]; ok {
			return collectionLevelPermission
		} else {
			permission := fmt.Sprintf("cluster.collection[%s].n1ql.index!%s", collectionLevel, op)
			p.permissions[collectionLevel] = isAllowed(creds, []string{permission}, nil)
			return p.permissions[collectionLevel]
		}
	}

	if checkAndAddBucketLevelPermission(bucket) {
		return true
	} else if checkAndAddScopeLevelPermission(bucket, scope) {
		return true
	} else if checkAndAddCollectionLevelPermission(bucket, scope, collection) {
		return true
	}
	return false
}

///////////////////////////////////////////////////////
// Cached LocalIndexMetadata and Stats
///////////////////////////////////////////////////////

func (m *requestHandlerContext) handleCachedLocalIndexMetadataRequest(w http.ResponseWriter, r *http.Request) {

	creds, ok := doAuth(r, w)
	if !ok {
		return
	}

	permissionsCache := initPermissionsCache()
	host := r.FormValue("host")
	host = strings.Trim(host, "\"")

	meta, err := m.getLocalMetadataFromDisk(host)
	if meta != nil && err == nil {
		newMeta := *meta
		newMeta.IndexDefinitions = make([]common.IndexDefn, 0, len(meta.IndexDefinitions))
		newMeta.IndexTopologies = make([]IndexTopology, 0, len(meta.IndexTopologies))

		for _, defn := range meta.IndexDefinitions {
			if permissionsCache.isAllowed(creds, defn.Bucket, defn.Scope, defn.Collection, "list") {
				newMeta.IndexDefinitions = append(newMeta.IndexDefinitions, defn)
			}
		}

		for _, topology := range meta.IndexTopologies {
			if permissionsCache.isAllowed(creds, topology.Bucket, topology.Scope, topology.Collection, "list") {
				newMeta.IndexTopologies = append(newMeta.IndexTopologies, topology)
			}
		}

		send(http.StatusOK, w, newMeta)

	} else {
		logging.Debugf("RequestHandler::handleCachedLocalIndexMetadataRequest: err %v", err)
		sendHttpError(w, " Unable to retrieve index metadata", http.StatusInternalServerError)
	}
}

func (m *requestHandlerContext) handleCachedStats(w http.ResponseWriter, r *http.Request) {

	_, ok := doAuth(r, w)
	if !ok {
		return
	}

	host := r.FormValue("host")
	host = strings.Trim(host, "\"")

	stats, err := m.getIndexStatsFromDisk(host)
	if stats != nil && err == nil {
		send(http.StatusOK, w, stats)
	} else {
		logging.Debugf("RequestHandler::handleCachedLocalIndexMetadataRequest: err %v", err)
		sendHttpError(w, " Unable to retrieve index metadata", http.StatusInternalServerError)
	}
}

///////////////////////////////////////////////////////
// Restore
///////////////////////////////////////////////////////

//
// Restore semantic:
// 1) Each index is associated with the <IndexDefnId, IndexerId>.  IndexDefnId is unique for each index defnition,
//    and IndexerId is unique among the index nodes.  Note that IndexDefnId cannot be reused.
// 2) Index defn exists for the given <IndexDefnId, IndexerId> in current repository.  No action will be applied during restore.
// 3) Index defn is deleted or missing in current repository.  Index Defn restored from backup if bucket exists.
//    - Index defn of the same <bucket, name> exists.   It will rename the index to <index name>_restore_<seqNo>
//    - Bucket does not exist.   It will restore an index defn with a non-existent bucket.
//
// TODO (Collections): Any changes necessary will be handled as part of Backup-Restore task
func (m *requestHandlerContext) handleRestoreIndexMetadataRequest(w http.ResponseWriter, r *http.Request) {

	creds, ok := doAuth(r, w)
	if !ok {
		return
	}

	permissionsCache := initPermissionsCache()
	// convert backup image into runtime data structure
	image := m.convertIndexMetadataRequest(r)
	if image == nil {
		send(http.StatusBadRequest, w, &RestoreResponse{Code: RESP_ERROR, Error: "Unable to process request input"})
		return
	}

	for _, localMeta := range image.Metadata {
		for _, topology := range localMeta.IndexTopologies {
			if !permissionsCache.isAllowed(creds, topology.Bucket, topology.Scope, topology.Collection, "write") {
				return
			}
		}

		for _, defn := range localMeta.IndexDefinitions {
			if !permissionsCache.isAllowed(creds, defn.Bucket, defn.Scope, defn.Collection, "write") {
				return
			}
		}
	}

	// Restore
	bucket := m.getBucket(r)
	logging.Infof("restore to target bucket %v", bucket)

	context := createRestoreContext(image, m.clusterUrl, bucket, nil, "", nil)
	hostIndexMap, err := context.computeIndexLayout()
	if err != nil {
		send(http.StatusInternalServerError, w, &RestoreResponse{Code: RESP_ERROR, Error: fmt.Sprintf("Unable to restore metadata.  Error=%v", err)})
	}

	if m.restoreIndexMetadataToNodes(hostIndexMap) {
		send(http.StatusOK, w, &RestoreResponse{Code: RESP_SUCCESS})
	} else {
		send(http.StatusInternalServerError, w, &RestoreResponse{Code: RESP_ERROR, Error: "Unable to restore metadata."})
	}
}
func (m *requestHandlerContext) restoreIndexMetadataToNodes(hostIndexMap map[string][]*common.IndexDefn) bool {

	var mu sync.Mutex
	var wg sync.WaitGroup

	errMap := make(map[string]bool)

	restoreIndexes := func(host string, indexes []*common.IndexDefn) {
		defer wg.Done()

		for _, index := range indexes {
			if !m.makeCreateIndexRequest(*index, host) {
				mu.Lock()
				defer mu.Unlock()

				errMap[host] = true
				return
			}
		}
	}

	for host, indexes := range hostIndexMap {
		wg.Add(1)
		go restoreIndexes(host, indexes)
	}

	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(errMap) != 0 {
		return false
	}

	return true
}

func (m *requestHandlerContext) makeCreateIndexRequest(defn common.IndexDefn, host string) bool {

	// deferred build for restore
	defn.Deferred = true

	req := IndexRequest{Version: uint64(1), Type: CREATE, Index: defn}
	body, err := json.Marshal(&req)
	if err != nil {
		logging.Errorf("requestHandler.makeCreateIndexRequest(): cannot marshall create index request %v", err)
		return false
	}

	bodybuf := bytes.NewBuffer(body)

	resp, err := postWithAuth(host+"/createIndex", "application/json", bodybuf)
	if err != nil {
		logging.Errorf("requestHandler.makeCreateIndexRequest(): create index request fails for %v/createIndex. Error=%v", host, err)
		return false
	}
	defer resp.Body.Close()

	response := new(IndexResponse)
	status := convertResponse(resp, response)
	if status == RESP_ERROR || response.Code == RESP_ERROR {
		logging.Errorf("requestHandler.makeCreateIndexRequest(): create index request fails. Error=%v", response.Error)
		return false
	}

	return true
}

//////////////////////////////////////////////////////
// Planner
///////////////////////////////////////////////////////

func (m *requestHandlerContext) handleIndexPlanRequest(w http.ResponseWriter, r *http.Request) {

	_, ok := doAuth(r, w)
	if !ok {
		return
	}

	stmts, err := m.getIndexPlan(r)

	if err == nil {
		send(http.StatusOK, w, stmts)
	} else {
		sendHttpError(w, err.Error(), http.StatusInternalServerError)
	}
}

func (m *requestHandlerContext) getIndexPlan(r *http.Request) (string, error) {

	plan, err := planner.RetrievePlanFromCluster(m.clusterUrl, nil)
	if err != nil {
		return "", errors.New(fmt.Sprintf("Fail to retreive index information from cluster.   Error=%v", err))
	}

	specs, err := m.convertIndexPlanRequest(r)
	if err != nil {
		return "", errors.New(fmt.Sprintf("Fail to read index spec from request.   Error=%v", err))
	}

	solution, err := planner.ExecutePlanWithOptions(plan, specs, true, "", "", 0, -1, -1, false, true)
	if err != nil {
		return "", errors.New(fmt.Sprintf("Fail to plan index.   Error=%v", err))
	}

	return planner.CreateIndexDDL(solution), nil
}

func (m *requestHandlerContext) convertIndexPlanRequest(r *http.Request) ([]*planner.IndexSpec, error) {

	var specs []*planner.IndexSpec

	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(r.Body); err != nil {
		logging.Debugf("RequestHandler::convertIndexPlanRequest: unable to read request body, err %v", err)
		return nil, err
	}

	logging.Debugf("requestHandler.convertIndexPlanRequest(): input %v", string(buf.Bytes()))

	if err := json.Unmarshal(buf.Bytes(), &specs); err != nil {
		logging.Debugf("RequestHandler::convertIndexPlanRequest: unable to unmarshall request body. Buf = %s, err %v", buf, err)
		return nil, err
	}

	return specs, nil
}

//////////////////////////////////////////////////////
// Storage Mode
///////////////////////////////////////////////////////

func (m *requestHandlerContext) handleIndexStorageModeRequest(w http.ResponseWriter, r *http.Request) {

	creds, ok := doAuth(r, w)
	if !ok {
		return
	}

	if !isAllowed(creds, []string{"cluster.settings!write"}, w) {
		return
	}

	// Override the storage mode for the local indexer.  Override will not take into effect until
	// indexer has restarted manually by administrator.   During indexer bootstrap, it will upgrade/downgrade
	// individual index to the override storage mode.
	value := r.FormValue("downgrade")
	if len(value) != 0 {
		downgrade, err := strconv.ParseBool(value)
		if err == nil {
			if downgrade {
				if common.GetStorageMode() == common.StorageMode(common.PLASMA) {

					nodeUUID, err := m.mgr.getMetadataRepo().GetLocalNodeUUID()
					if err != nil {
						logging.Infof("RequestHandler::handleIndexStorageModeRequest: unable to identify nodeUUID.  Cannot downgrade.")
						send(http.StatusOK, w, "Unable to identify nodeUUID.  Cannot downgrade.")
						return
					}

					mc.PostIndexerStorageModeOverride(string(nodeUUID), common.ForestDB)
					logging.Infof("RequestHandler::handleIndexStorageModeRequest: set override storage mode to forestdb")
					send(http.StatusOK, w, "downgrade storage mode to forestdb after indexer restart.")
				} else {
					logging.Infof("RequestHandler::handleIndexStorageModeRequest: local storage mode is not plasma.  Cannot downgrade.")
					send(http.StatusOK, w, "Indexer storage mode is not plasma.  Cannot downgrade.")
				}
			} else {
				nodeUUID, err := m.mgr.getMetadataRepo().GetLocalNodeUUID()
				if err != nil {
					logging.Infof("RequestHandler::handleIndexStorageModeRequest: unable to identify nodeUUID. Cannot disable storage mode downgrade.")
					send(http.StatusOK, w, "Unable to identify nodeUUID.  Cannot disable storage mode downgrade.")
					return
				}

				mc.PostIndexerStorageModeOverride(string(nodeUUID), "")
				logging.Infof("RequestHandler::handleIndexStorageModeRequst: unset storage mode override")
				send(http.StatusOK, w, "storage mode downgrade is disabled")
			}
		} else {
			sendHttpError(w, err.Error(), http.StatusBadRequest)
		}
	} else {
		sendHttpError(w, "missing argument `override`", http.StatusBadRequest)
	}
}

//////////////////////////////////////////////////////
// Planner
///////////////////////////////////////////////////////

func (m *requestHandlerContext) handlePlannerRequest(w http.ResponseWriter, r *http.Request) {

	creds, ok := doAuth(r, w)
	if !ok {
		return
	}

	if !isAllowed(creds, []string{"cluster.settings!write"}, w) {
		return
	}

	value := r.FormValue("excludeNode")
	if value == "in" || value == "out" || value == "inout" || len(value) == 0 {
		m.mgr.SetLocalValue("excludeNode", value)
		send(http.StatusOK, w, "OK")
	} else {
		sendHttpError(w, "value must be in, out or inout", http.StatusBadRequest)
	}
}

//////////////////////////////////////////////////////
// Alter Index
///////////////////////////////////////////////////////

func (m *requestHandlerContext) handleListLocalReplicaCountRequest(w http.ResponseWriter, r *http.Request) {

	creds, ok := doAuth(r, w)
	if !ok {
		return
	}

	result, err := m.getLocalReplicaCount(creds)
	if err == nil {
		send(http.StatusOK, w, result)
	} else {
		logging.Debugf("RequestHandler::handleListReplicaCountRequest: err %v", err)
		sendHttpError(w, " Unable to retrieve index metadata", http.StatusInternalServerError)
	}
}

func (m *requestHandlerContext) getLocalReplicaCount(creds cbauth.Creds) (map[common.IndexDefnId]common.Counter, error) {

	result := make(map[common.IndexDefnId]common.Counter)

	repo := m.mgr.getMetadataRepo()
	iter, err := repo.NewIterator()
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	var defn *common.IndexDefn
	permissionsCache := initPermissionsCache()

	_, defn, err = iter.Next()
	for err == nil {
		if !permissionsCache.isAllowed(creds, defn.Bucket, defn.Scope, defn.Collection, "list") {
			return nil, fmt.Errorf("Permission denied on reading metadata for keyspace %v:%v:%v", defn.Bucket, defn.Scope, defn.Collection)
		}

		var numReplica *common.Counter
		numReplica, err = GetLatestReplicaCount(defn)
		if err != nil {
			return nil, fmt.Errorf("Fail to retreive replica count.  Error: %v", err)
		}

		result[defn.DefnId] = *numReplica
		_, defn, err = iter.Next()
	}

	return result, nil
}

///////////////////////////////////////////////////////
// Utility
///////////////////////////////////////////////////////

func sendIndexResponseWithError(status int, w http.ResponseWriter, msg string) {
	res := &IndexResponse{Code: RESP_ERROR, Error: msg}
	send(status, w, res)
}

func sendIndexResponse(w http.ResponseWriter) {
	result := &IndexResponse{Code: RESP_SUCCESS}
	send(http.StatusOK, w, result)
}

func send(status int, w http.ResponseWriter, res interface{}) {

	header := w.Header()
	header["Content-Type"] = []string{"application/json"}

	if buf, err := json.Marshal(res); err == nil {
		w.WriteHeader(status)
		logging.Tracef("RequestHandler::sendResponse: sending response back to caller. %v", logging.TagStrUD(buf))
		w.Write(buf)
	} else {
		// note : buf is nil if err != nil
		logging.Debugf("RequestHandler::sendResponse: fail to marshall response back to caller. %s", err)
		sendHttpError(w, "RequestHandler::sendResponse: Unable to marshall response", http.StatusInternalServerError)
	}
}

func sendHttpError(w http.ResponseWriter, reason string, code int) {
	http.Error(w, reason, code)
}

func convertResponse(r *http.Response, resp interface{}) string {

	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(r.Body); err != nil {
		logging.Debugf("RequestHandler::convertResponse: unable to read request body, err %v", err)
		return RESP_ERROR
	}

	if err := json.Unmarshal(buf.Bytes(), resp); err != nil {
		logging.Debugf("convertResponse: unable to unmarshall response body. Buf = %s, err %v", buf, err)
		return RESP_ERROR
	}

	return RESP_SUCCESS
}

func doAuth(r *http.Request, w http.ResponseWriter) (cbauth.Creds, bool) {

	creds, valid, err := common.IsAuthValid(r)
	if err != nil {
		sendIndexResponseWithError(http.StatusInternalServerError, w, err.Error())
		return nil, false
	} else if valid == false {
		w.WriteHeader(401)
		w.Write([]byte("401 Unauthorized\n"))
		return nil, false
	}

	return creds, true
}

// TODO: This function shouldn't always return IndexResponse on error in
// verifying auth. It should depend on the caller.
func isAllowed(creds cbauth.Creds, permissions []string, w http.ResponseWriter) bool {

	allow := false
	err := error(nil)

	for _, permission := range permissions {
		allow, err = creds.IsAllowed(permission)
		if allow && err == nil {
			break
		}
	}

	if err != nil {
		if w != nil {
			sendIndexResponseWithError(http.StatusInternalServerError, w, err.Error())
		}
		return false
	}

	if !allow {
		if w != nil {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(http.StatusText(http.StatusUnauthorized)))
		}
		return false
	}

	return true
}

func getWithAuth(url string) (*http.Response, error) {
	params := &security.RequestParams{Timeout: time.Duration(10) * time.Second}
	return security.GetWithAuth(url, params)
}

func postWithAuth(url string, bodyType string, body io.Reader) (*http.Response, error) {
	params := &security.RequestParams{Timeout: time.Duration(10) * time.Second}
	return security.PostWithAuth(url, bodyType, body, params)
}

func findTopologyByCollection(topologies []IndexTopology, bucket, scope, collection string) *IndexTopology {

	for _, topology := range topologies {
		t := &topology
		t.SetCollectionDefaults()
		if t.Bucket == bucket && t.Scope == scope && t.Collection == collection {
			return t
		}
	}

	return nil
}

///////////////////////////////////////////////////////
// indexStatusSorter
///////////////////////////////////////////////////////

func (s indexStatusSorter) Len() int {
	return len(s)
}

func (s indexStatusSorter) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

// TODO (Collections): Revisit scalabity of sorting with large number of indexes
// Also, check if sorting still necessary.
func (s indexStatusSorter) Less(i, j int) bool {
	if s[i].Name < s[j].Name {
		return true
	}

	if s[i].Name > s[j].Name {
		return false
	}

	if s[i].Collection < s[j].Collection {
		return true
	}

	if s[i].Collection > s[j].Collection {
		return false
	}

	if s[i].Scope < s[j].Scope {
		return true
	}

	if s[i].Scope > s[j].Scope {
		return false
	}

	return s[i].Bucket < s[j].Bucket
}

///////////////////////////////////////////////////////
// retrieve / persist cached local index metadata
///////////////////////////////////////////////////////

func (m *requestHandlerContext) getLocalMetadataForNode(addr string, host string, cinfo *common.ClusterInfoCache) (*LocalIndexMetadata, bool, error) {

	meta, err := m.getLocalMetadataFromREST(addr, host)
	if err == nil {
		return meta, true, nil
	}

	if cinfo.GetClusterVersion() >= common.INDEXER_65_VERSION {
		var latest *LocalIndexMetadata
		nids := cinfo.GetNodesByServiceType(common.INDEX_HTTP_SERVICE)
		for _, nid := range nids {
			addr, err1 := cinfo.GetServiceAddress(nid, common.INDEX_HTTP_SERVICE)
			if err1 == nil {
				cached, err1 := m.getCachedLocalMetadataFromREST(addr, host)
				if cached != nil && err1 == nil {
					if latest == nil || cached.Timestamp > latest.Timestamp {
						latest = cached
					}
				}
			}
		}

		if latest != nil {
			return latest, false, nil
		}
	}

	return nil, false, err
}

func (m *requestHandlerContext) getLocalMetadataFromREST(addr string, hostname string) (*LocalIndexMetadata, error) {

	resp, err := getWithAuth(addr + "/getLocalIndexMetadata")
	defer func() {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
	}()

	if err == nil {
		localMeta := new(LocalIndexMetadata)
		if status := convertResponse(resp, localMeta); status == RESP_SUCCESS {

			m.mutex.Lock()
			filename := host2file(hostname)
			if _, ok := m.metaCache[filename]; ok {
				logging.Debugf("getLocalMetadataFromREST: remove metadata form in-memory cache %v", filename)
				delete(m.metaCache, filename)
			}
			m.mutex.Unlock()

			return localMeta, nil
		}

		err = fmt.Errorf("Fail to unmarshal response from %v", hostname)
	}

	return nil, err
}

func (m *requestHandlerContext) getCachedLocalMetadataFromREST(addr string, host string) (*LocalIndexMetadata, error) {

	resp, err := getWithAuth(fmt.Sprintf("%v/getCachedLocalIndexMetadata?host=\"%v\"", addr, host))
	defer func() {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
	}()

	if err == nil {
		localMeta := new(LocalIndexMetadata)
		if status := convertResponse(resp, localMeta); status == RESP_SUCCESS {
			return localMeta, nil
		}

		err = fmt.Errorf("Fail to unmarshal response from %v", host)
	}

	return nil, err
}

func (m *requestHandlerContext) getLocalMetadataFromDisk(hostname string) (*LocalIndexMetadata, error) {

	filename := host2file(hostname)

	m.mutex.RLock()
	if meta, ok := m.metaCache[filename]; ok && meta != nil {
		logging.Debugf("getLocalMetadataFromDisk(): found metadata from in-memory cache %v", filename)
		m.mutex.RUnlock()
		return meta, nil
	}
	m.mutex.RUnlock()

	filepath := path.Join(m.metaDir, filename)

	content, err := ioutil.ReadFile(filepath)
	if err != nil {
		logging.Errorf("getLocalMetadataFromDisk(): fail to read metadata from file %v.  Error %v", filepath, err)
		return nil, err
	}

	localMeta := new(LocalIndexMetadata)
	if err := json.Unmarshal(content, localMeta); err != nil {
		logging.Errorf("getLocalMetadataFromDisk(): fail to unmarshal metadata from file %v.  Error %v", filepath, err)
		return nil, err
	}

	m.mutex.Lock()
	logging.Debugf("getLocalMetadataFromDisk(): save metadata to in-memory cache %v", filename)
	m.metaCache[filename] = localMeta
	m.mutex.Unlock()

	return localMeta, nil
}

func (m *requestHandlerContext) saveLocalMetadataToDisk(hostname string, meta *LocalIndexMetadata) error {

	filename := host2file(hostname)
	filepath := path.Join(m.metaDir, filename)
	temp := path.Join(m.metaDir, filename+".tmp")

	content, err := json.Marshal(meta)
	if err != nil {
		logging.Errorf("saveLocalMetadatasToDisk(): fail to marshal metadata to file %v.  Error %v", filepath, err)
		return err
	}

	err = ioutil.WriteFile(temp, content, 0755)
	if err != nil {
		logging.Errorf("saveLocalMetadataToDisk(): fail to save metadata to file %v.  Error %v", temp, err)
		return err
	}

	err = os.Rename(temp, filepath)
	if err != nil {
		logging.Errorf("saveLocalMetadataToDisk(): fail to rename metadata to file %v.  Error %v", filepath, err)
		return err
	}

	logging.Debugf("saveLocalMetadataToDisk(): successfully written metadata to disk for %v", filename)

	return nil
}

func (m *requestHandlerContext) cleanupLocalMetadataOnDisk(hostnames []string) {

	filenames := make([]string, len(hostnames))
	for i, hostname := range hostnames {
		filenames[i] = host2file(hostname)
	}

	files, err := ioutil.ReadDir(m.metaDir)
	if err != nil {
		logging.Errorf("cleanupLocalMetadataOnDisk(): fail to read directory %v.  Error %v", m.metaDir, err)
		return
	}

	for _, file := range files {
		filename := file.Name()

		found := false
		for _, filename2 := range filenames {
			if filename2 == filename {
				found = true
			}
		}

		if !found {
			filepath := path.Join(m.metaDir, filename)
			if err := os.RemoveAll(filepath); err != nil {
				logging.Errorf("cleanupLocalMetadataOnDisk(): fail to remove file %v.  Error %v", filepath, err)
			}

			logging.Debugf("cleanupLocalMetadataOnDisk(): succesfully removing file %v from cache.", filepath)

			m.mutex.Lock()
			if _, ok := m.metaCache[filename]; ok {
				logging.Debugf("cleanupMetadataFromDisk: remove metadata form in-memory cache %v", filename)
				delete(m.metaCache, filename)
			}
			m.mutex.Unlock()
		}
	}
}

///////////////////////////////////////////////////////
// retrieve / persist cached index stats
///////////////////////////////////////////////////////

func (m *requestHandlerContext) getStatsForNode(addr string, host string, cinfo *common.ClusterInfoCache) (*common.Statistics, bool, error) {

	stats, err := m.getStatsFromREST(addr, host)
	if err == nil {
		return stats, true, nil
	}

	if cinfo.GetClusterVersion() >= common.INDEXER_65_VERSION {
		var latest *common.Statistics
		nids := cinfo.GetNodesByServiceType(common.INDEX_HTTP_SERVICE)
		for _, nid := range nids {
			addr, err1 := cinfo.GetServiceAddress(nid, common.INDEX_HTTP_SERVICE)
			if err1 == nil {
				cached, err1 := m.getCachedStatsFromREST(addr, host)
				if cached != nil && err1 == nil {
					if latest == nil {
						latest = cached
						continue
					}

					ts1 := latest.Get("timestamp")
					if ts1 == nil {
						latest = cached
						continue
					}

					ts2 := cached.Get("timestamp")
					if ts2 == nil {
						continue
					}

					t1, ok1 := ts1.(float64)
					t2, ok2 := ts2.(float64)

					if ok1 && ok2 {
						if t2 > t1 {
							latest = cached
						}
					}
				}
			}
		}

		if latest != nil {
			return latest, false, nil
		}
	}

	return nil, false, err
}

func (m *requestHandlerContext) getStatsFromREST(addr string, hostname string) (*common.Statistics, error) {

	resp, err := getWithAuth(addr + "/stats?async=true&consumerFilter=indexStatus")
	defer func() {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
	}()

	if err == nil {
		stats := new(common.Statistics)
		if status := convertResponse(resp, stats); status == RESP_SUCCESS {

			m.mutex.Lock()
			filename := host2file(hostname)
			if _, ok := m.statsCache[filename]; ok {
				logging.Debugf("getStatsFromREST: remove stats from in-memory cache %v", filename)
				delete(m.statsCache, filename)
			}
			m.mutex.Unlock()

			return stats, nil
		}

		err = fmt.Errorf("Fail to unmarshal response from %v", hostname)
	}

	return nil, err
}

func (m *requestHandlerContext) getCachedStatsFromREST(addr string, host string) (*common.Statistics, error) {

	resp, err := getWithAuth(fmt.Sprintf("%v/getCachedStats?host=\"%v\"", addr, host))
	defer func() {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
	}()

	if err == nil {
		stats := new(common.Statistics)
		if status := convertResponse(resp, stats); status == RESP_SUCCESS {
			return stats, nil
		}

		err = fmt.Errorf("Fail to unmarshal response from %v", host)
	}

	return nil, err
}

func (m *requestHandlerContext) getIndexStatsFromDisk(hostname string) (*common.Statistics, error) {

	filename := host2file(hostname)

	m.mutex.RLock()
	if stats, ok := m.statsCache[filename]; ok && stats != nil {
		logging.Debugf("getIndexStatsFromDisk(): found stats from in-memory cache %v", filename)
		m.mutex.RUnlock()
		return stats, nil
	}
	m.mutex.RUnlock()

	filepath := path.Join(m.statsDir, filename)

	content, err := ioutil.ReadFile(filepath)
	if err != nil {
		logging.Errorf("getIndexStatsFromDisk(): fail to read stats from file %v.  Error %v", filepath, err)
		return nil, err
	}

	stats := new(common.Statistics)
	if err := json.Unmarshal(content, stats); err != nil {
		logging.Errorf("getIndexStatsFromDisk(): fail to unmarshal stats from file %v.  Error %v", filepath, err)
		return nil, err
	}

	m.mutex.Lock()
	m.statsCache[filename] = stats
	logging.Debugf("getIndexStatsFromDisk(): save stats to in-memory cache %v", filename)
	m.mutex.Unlock()

	return stats, nil
}

func (m *requestHandlerContext) saveIndexStatsToDisk(hostname string, stats *common.Statistics) error {

	filename := host2file(hostname)
	filepath := path.Join(m.statsDir, filename)
	temp := path.Join(m.statsDir, filename+".tmp")

	content, err := json.Marshal(stats)
	if err != nil {
		logging.Errorf("saveIndexStatsToDisk(): fail to marshal stats to file %v.  Error %v", filepath, err)
		return err
	}

	err = ioutil.WriteFile(temp, content, 0755)
	if err != nil {
		logging.Errorf("saveIndexStatsToDisk(): fail to save stats to file %v.  Error %v", temp, err)
		return err
	}

	err = os.Rename(temp, filepath)
	if err != nil {
		logging.Errorf("saveIndexStatsToDisk(): fail to rename stats to file %v.  Error %v", filepath, err)
		return err
	}

	logging.Debugf("saveIndexStatsToDisk(): successfully written stats to disk for %v", filename)

	return nil
}

func (m *requestHandlerContext) cleanupIndexStatsOnDisk(hostnames []string) {

	filenames := make([]string, len(hostnames))
	for i, hostname := range hostnames {
		filenames[i] = host2file(hostname)
	}

	files, err := ioutil.ReadDir(m.statsDir)
	if err != nil {
		logging.Errorf("cleanupStatsOnDisk(): fail to read directory %v.  Error %v", m.statsDir, err)
		return
	}

	for _, file := range files {
		filename := file.Name()

		found := false
		for _, filename2 := range filenames {
			if filename2 == filename {
				found = true
			}
		}

		if !found {
			filepath := path.Join(m.statsDir, filename)
			if err := os.RemoveAll(filepath); err != nil {
				logging.Errorf("cleanupStatsOnDisk(): fail to remove file %v.  Error %v", filepath, err)
			}

			logging.Debugf("cleanupIndexStatsOnDisk(): succesfully removing file %v from cache.", filepath)

			m.mutex.Lock()
			if _, ok := m.statsCache[filename]; ok {
				logging.Debugf("cleanupStatsOnDisk: remove stats from in-memory cache %v", filename)
				delete(m.statsCache, filename)
			}
			m.mutex.Unlock()
		}
	}
}

///////////////////////////////////////////////////////
// persistor
///////////////////////////////////////////////////////

func (m *requestHandlerContext) runPersistor() {

	updateMeta := func(v map[string]*LocalIndexMetadata) {
		hostnames := make([]string, 0, len(v))

		for host, meta := range v {
			if meta != nil {
				m.saveLocalMetadataToDisk(host, meta)
			}
			hostnames = append(hostnames, host)
		}

		m.cleanupLocalMetadataOnDisk(hostnames)
	}

	updateStats := func(v map[string]*common.Statistics) {
		hostnames := make([]string, 0, len(v))

		for host, stats := range v {
			if stats != nil {
				m.saveIndexStatsToDisk(host, stats)
			}
			hostnames = append(hostnames, host)
		}

		m.cleanupIndexStatsOnDisk(hostnames)
	}

	for {
		select {
		case v, ok := <-m.metaCh:
			if !ok {
				return
			}

			for len(m.metaCh) > 0 {
				v = <-m.metaCh
			}

			updateMeta(v)

		case v, ok := <-m.statsCh:
			if !ok {
				return
			}

			for len(m.statsCh) > 0 {
				v = <-m.statsCh
			}

			updateStats(v)

		case <-m.doneCh:
			logging.Infof("request_handler persistor exits")
			return
		}
	}
}

func (m *requestHandlerContext) handleScheduleCreateRequest(w http.ResponseWriter, r *http.Request) {
	creds, ok := doAuth(r, w)
	if !ok {
		return
	}

	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(r.Body); err != nil {
		logging.Debugf("RequestHandler::handleScheduleCreateRequest: unable to read request body, err %v", err)
		send(http.StatusBadRequest, w, "Unable to read request body")
		return
	}

	req := &client.ScheduleCreateRequest{}
	if err := json.Unmarshal(buf.Bytes(), req); err != nil {
		logging.Debugf("RequestHandler::handleScheduleCreateRequest: unable to unmarshall request body. Buf = %s, err %v", logging.TagStrUD(buf), err)
		send(http.StatusBadRequest, w, "Unable to unmarshall request body")
		return
	}

	if req.Definition.DefnId == common.IndexDefnId(0) {
		logging.Warnf("RequestHandler::handleScheduleCreateRequest: empty index definition")
		send(http.StatusBadRequest, w, "Empty index definition")
		return
	}

	permission := fmt.Sprintf("cluster.collection[%s:%s:%s].n1ql.index!create", req.Definition.Bucket, req.Definition.Scope, req.Definition.Collection)
	if !isAllowed(creds, []string{permission}, w) {
		send(http.StatusForbidden, w, "Specified user cannot create an index on the bucket")
		return
	}

	err := m.processScheduleCreateRequest(req)
	if err != nil {
		msg := fmt.Sprintf("Error in processing schedule create token: %v", err)
		logging.Errorf("RequestHandler::handleScheduleCreateRequest: %v", msg)
		send(http.StatusInternalServerError, w, msg)
		return
	}

	send(http.StatusOK, w, "OK")
}

func (m *requestHandlerContext) validateScheduleCreateRequst(req *client.ScheduleCreateRequest) (string, string, string, error) {

	// Check for all possible fail-fast situations. Fail scheduling of index
	// creation if any of the required preconditions are not satisfied.

	defn := req.Definition

	if common.GetBuildMode() != common.ENTERPRISE {
		if defn.NumReplica != 0 {
			err := errors.New("Index Replica not supported in non-Enterprise Edition")
			return "", "", "", err
		}
		if common.IsPartitioned(defn.PartitionScheme) {
			err := errors.New("Index Partitining is not supported in non-Enterprise Edition")
			return "", "", "", err
		}
	}

	// Check for bucket, scope, collection to be present.
	var bucketUUID, scopeId, collectionId string
	var err error

	bucketUUID, err = m.getBucketUUID(defn.Bucket)
	if err != nil {
		return "", "", "", err
	}

	if bucketUUID == common.BUCKET_UUID_NIL {
		return "", "", "", common.ErrBucketNotFound
	}

	scopeId, collectionId, err = m.getScopeAndCollectionID(defn.Bucket, defn.Scope, defn.Collection)
	if err != nil {
		return "", "", "", err
	}

	if scopeId == collections.SCOPE_ID_NIL {
		return "", "", "", common.ErrScopeNotFound
	}

	if collectionId == collections.COLLECTION_ID_NIL {
		return "", "", "", common.ErrCollectionNotFound
	}

	if common.GetStorageMode() == common.NOT_SET {
		return "", "", "", fmt.Errorf("Please Set Indexer Storage Mode Before Create Index")
	}

	err = m.validateStorageMode(&defn)
	if err != nil {
		return "", "", "", err
	}

	// TODO: Check indexer state to be active

	var ephimeral bool
	ephimeral, err = m.isEphemeral(defn.Bucket)
	if err != nil {
		return "", "", "", err
	}

	if ephimeral && common.GetStorageMode() != common.MOI {
		return "", "", "", fmt.Errorf("Bucket %v is Ephemeral but GSI storage is not MOI", defn.Bucket)
	}

	return bucketUUID, scopeId, collectionId, nil
}

func (m *requestHandlerContext) isEphemeral(bucket string) (bool, error) {
	var cinfo *common.ClusterInfoCache
	cinfo = m.mgr.reqcic.GetClusterInfoCache()

	if cinfo == nil {
		return false, errors.New("ClusterInfoCache unavailable in IndexManager")
	}

	cinfo.RLock()
	defer cinfo.RUnlock()

	return cinfo.IsEphemeral(bucket)
}

func (m *requestHandlerContext) validateStorageMode(defn *common.IndexDefn) error {

	//if no index_type has been specified
	if strings.ToLower(string(defn.Using)) == "gsi" {
		if common.GetStorageMode() != common.NOT_SET {
			//if there is a storage mode, default to that
			defn.Using = common.IndexType(common.GetStorageMode().String())
		} else {
			//default to plasma
			defn.Using = common.PlasmaDB
		}
	} else {
		if common.IsValidIndexType(string(defn.Using)) {
			defn.Using = common.IndexType(strings.ToLower(string(defn.Using)))
		} else {
			err := fmt.Sprintf("Create Index fails. Reason = Unsupported Using Clause %v", string(defn.Using))
			return errors.New(err)
		}
	}

	if common.IsPartitioned(defn.PartitionScheme) {
		if defn.Using != common.PlasmaDB && defn.Using != common.MemDB && defn.Using != common.MemoryOptimized {
			err := fmt.Sprintf("Create Index fails. Reason = Cannot create partitioned index using %v", string(defn.Using))
			return errors.New(err)
		}
	}

	if common.IndexTypeToStorageMode(defn.Using) != common.GetStorageMode() {
		return fmt.Errorf("Cannot Create Index with Using %v. Indexer Storage Mode %v",
			defn.Using, common.GetStorageMode())
	}

	return nil
}

// This function returns an error if it cannot connect for fetching bucket info.
// It returns BUCKET_UUID_NIL (err == nil) if bucket does not exist.
//
func (m *requestHandlerContext) getBucketUUID(bucket string) (string, error) {
	count := 0
RETRY:
	uuid, err := common.GetBucketUUID(m.clusterUrl, bucket)
	if err != nil && count < 5 {
		count++
		time.Sleep(time.Duration(100) * time.Millisecond)
		goto RETRY
	}

	if err != nil {
		return common.BUCKET_UUID_NIL, err
	}

	return uuid, nil
}

// This function returns an error if it cannot connect for fetching manifest info.
// It returns SCOPE_ID_NIL, COLLECTION_ID_NIL (err == nil) if scope, collection does
// not exist.
//
func (m *requestHandlerContext) getScopeAndCollectionID(bucket, scope, collection string) (string, string, error) {
	count := 0
RETRY:
	scopeId, colldId, err := common.GetScopeAndCollectionID(m.clusterUrl, bucket, scope, collection)
	if err != nil && count < 5 {
		count++
		time.Sleep(time.Duration(100) * time.Millisecond)
		goto RETRY
	}

	if err != nil {
		return "", "", err
	}

	return scopeId, colldId, nil
}

func (m *requestHandlerContext) processScheduleCreateRequest(req *client.ScheduleCreateRequest) error {
	bucketUUID, scopeId, collectionId, err := m.validateScheduleCreateRequst(req)
	if err != nil {
		logging.Errorf("requestHandlerContext: Error in validateScheduleCreateRequst %v", err)
		return err
	}

	err = mc.PostScheduleCreateToken(req.Definition, req.Plan, bucketUUID, scopeId, collectionId,
		req.IndexerId, time.Now().UnixNano())
	if err != nil {
		logging.Errorf("requestHandlerContext: Error in PostScheduleCreateToken %v", err)
		return err
	}

	return nil
}

//
// Handle restore of a bucket.
//
func (m *requestHandlerContext) bucketRestoreHandler(bucket, include, exclude string, r *http.Request) (int, string) {

	filters, filterType, err := getFilters(r, bucket)
	if err != nil {
		logging.Errorf("RequestHandler::bucketRestoreHandler: err in getFilters %v", err)
		return http.StatusBadRequest, err.Error()
	}

	remap, err1 := getRestoreRemapParam(r)
	if err1 != nil {
		logging.Errorf("RequestHandler::bucketRestoreHandler: err in getRestoreRemapParam %v", err1)
		return http.StatusBadRequest, err1.Error()
	}

	logging.Debugf("bucketRestoreHandler: remap %v", remap)

	image := m.convertIndexMetadataRequest(r)
	if image == nil {
		return http.StatusBadRequest, "Unable to process request input"
	}

	context := createRestoreContext(image, m.clusterUrl, bucket, filters, filterType, remap)
	hostIndexMap, err2 := context.computeIndexLayout()
	if err2 != nil {
		logging.Errorf("RequestHandler::bucketRestoreHandler: err in computeIndexLayout %v", err2)
		return http.StatusInternalServerError, err2.Error()
	}

	if !m.restoreIndexMetadataToNodes(hostIndexMap) {
		return http.StatusInternalServerError, "Unable to restore metadata."
	}

	return http.StatusOK, ""
}

//
// Handle backup of a bucket.
// Note that this function does not verify auths or RBAC
//
func (m *requestHandlerContext) bucketBackupHandler(bucket, include, exclude string,
	r *http.Request) (*ClusterIndexMetadata, error) {

	cinfo, err := m.mgr.FetchNewClusterInfoCache()
	if err != nil {
		return nil, err
	}

	// find all nodes that has a index http service
	nids := cinfo.GetNodesByServiceType(common.INDEX_HTTP_SERVICE)

	clusterMeta := &ClusterIndexMetadata{Metadata: make([]LocalIndexMetadata, len(nids))}

	respMap := make(map[common.NodeId]*http.Response)
	errMap := make(map[common.NodeId]error)

	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, nid := range nids {

		getLocalMeta := func(nid common.NodeId) {
			defer wg.Done()

			cinfo.RLock()
			defer cinfo.RUnlock()

			addr, err := cinfo.GetServiceAddress(nid, common.INDEX_HTTP_SERVICE)
			if err == nil {
				url := "/getLocalIndexMetadata?bucket=" + bucket
				if len(include) != 0 {
					url += "&include=" + include
				}

				if len(exclude) != 0 {
					url += "&exclude=" + exclude
				}

				resp, err := getWithAuth(addr + url)
				mu.Lock()
				defer mu.Unlock()

				if err != nil {
					logging.Debugf("RequestHandler::bucketBackupHandler: Error while retrieving %v with auth %v", addr+"/getLocalIndexMetadata", err)
					errMap[nid] = errors.New(fmt.Sprintf("Fail to retrieve index definition from url %s: err = %v", addr, err))
					respMap[nid] = nil
				} else {
					respMap[nid] = resp
				}
			} else {
				mu.Lock()
				defer mu.Unlock()

				errMap[nid] = errors.New(fmt.Sprintf("Fail to retrieve http endpoint for index node"))
			}
		}

		wg.Add(1)
		go getLocalMeta(nid)
	}

	wg.Wait()

	mu.Lock()
	defer mu.Unlock()

	for _, resp := range respMap {
		if resp != nil && resp.Body != nil {
			defer resp.Body.Close()
		}
	}

	if len(errMap) != 0 {
		for _, err := range errMap {
			return nil, err
		}
	}

	cinfo.RLock()
	defer cinfo.RUnlock()

	i := 0
	for nid, resp := range respMap {

		localMeta := new(LocalIndexMetadata)
		status := convertResponse(resp, localMeta)
		if status == RESP_ERROR {
			addr, err := cinfo.GetServiceAddress(nid, common.INDEX_HTTP_SERVICE)
			if err != nil {
				return nil, errors.New(fmt.Sprintf("Fail to retrieve local metadata from node id %v.", nid))
			} else {
				return nil, errors.New(fmt.Sprintf("Fail to retrieve local metadata from url %v.", addr))
			}
		}

		newLocalMeta := LocalIndexMetadata{
			IndexerId:   localMeta.IndexerId,
			NodeUUID:    localMeta.NodeUUID,
			StorageMode: localMeta.StorageMode,
		}

		for _, topology := range localMeta.IndexTopologies {
			newLocalMeta.IndexTopologies = append(newLocalMeta.IndexTopologies, topology)
		}

		for _, defn := range localMeta.IndexDefinitions {
			newLocalMeta.IndexDefinitions = append(newLocalMeta.IndexDefinitions, defn)
		}

		clusterMeta.Metadata[i] = newLocalMeta
		i++
	}

	filters, filterType, err := getFilters(r, bucket)
	if err != nil {
		return nil, err
	}

	schedTokens, err1 := getSchedCreateTokens(bucket, filters, filterType)
	if err1 != nil {
		return nil, err1
	}

	clusterMeta.SchedTokens = schedTokens

	return clusterMeta, nil
}

func getSchedCreateTokens(bucket string, filters map[string]bool, filterType string) (
	map[common.IndexDefnId]*mc.ScheduleCreateToken, error) {

	schedTokensMap := make(map[common.IndexDefnId]*mc.ScheduleCreateToken)
	stopSchedTokensMap := make(map[common.IndexDefnId]bool)

	scheduleTokens, err := mc.ListAllScheduleCreateTokens()
	if err != nil {
		return nil, err
	}

	stopScheduleTokens, err1 := mc.ListAllStopScheduleCreateTokens()
	if err1 != nil {
		return nil, err1
	}

	for _, token := range stopScheduleTokens {
		stopSchedTokensMap[token.DefnId] = true
	}

	for _, token := range scheduleTokens {
		if _, ok := stopSchedTokensMap[token.Definition.DefnId]; !ok {
			if !applyFilters(bucket, token.Definition.Bucket, token.Definition.Scope,
				token.Definition.Collection, "", filters, filterType) {

				continue
			}

			schedTokensMap[token.Definition.DefnId] = token
		}
	}

	return schedTokensMap, nil
}

func (m *requestHandlerContext) authorizeBucketRequest(w http.ResponseWriter,
	r *http.Request, creds cbauth.Creds, bucket, include, exclude string) bool {

	// Basic RBAC.
	// 1. If include filter is specified, verify user has permissions to access
	//    indexes created on all component scopes and collections.
	// 2. If include filter is not specified, verify user has permissions to
	//    access the indexes for the bucket.
	//
	// During backup, Local index metadata call can peform RBAC based filtering.
	// So, in case of unauthorized access to a specific scope / collection,
	// backup service will get an appropriate error.

	var op string
	switch r.Method {
	case "GET":
		op = "list"

	case "POST":
		op = "create"

	default:
		send(http.StatusBadRequest, w, fmt.Sprintf("Unsupported method %v", r.Method))
		return false
	}

	if len(include) == 0 {
		switch r.Method {
		case "GET":
			permission := fmt.Sprintf("cluster.bucket[%s].n1ql.index!%s", bucket, op)
			if !isAllowed(creds, []string{permission}, w) {
				return false
			}

		case "POST":
			permission := fmt.Sprintf("cluster.bucket[%s].n1ql.index!%s", bucket, op)
			if !isAllowed(creds, []string{permission}, w) {
				// TODO: If bucket level verification fails, then as a best effort,
				// iterate over restore metadata and verify for each scope/collection.
				// This will be needed only if backup was performed by a user using
				// include filter (or without any filter) and restore is being
				// performed by a another user (with less privileges) using an exclude
				// filter. This scenario seems unlikely.
				return false
			}

		default:
			send(http.StatusBadRequest, w, fmt.Sprintf("Unsupported method %v", r.Method))
			return false
		}
	} else {
		incls := strings.Split(include, ",")
		for _, incl := range incls {
			inc := strings.Split(incl, ".")
			if len(inc) == 1 {
				scope := fmt.Sprintf("%s:%s", bucket, inc[0])
				permission := fmt.Sprintf("cluster.scope[%s].n1ql.index!%s", scope, op)
				if !isAllowed(creds, []string{permission}, w) {
					return false
				}
			} else if len(inc) == 2 {
				collection := fmt.Sprintf("%s:%s:%s", bucket, inc[0], inc[1])
				permission := fmt.Sprintf("cluster.collection[%s].n1ql.index!%s", collection, op)
				if !isAllowed(creds, []string{permission}, w) {
					return false
				}
			} else {
				send(http.StatusBadRequest, w, fmt.Sprintf("Malformed url %v, include %v", r.URL.Path, include))
				return false
			}
		}
	}

	return true
}

func (m *requestHandlerContext) bucketReqHandler(w http.ResponseWriter, r *http.Request, creds cbauth.Creds) {
	url := filepath.Clean(r.URL.Path)
	logging.Debugf("bucketReqHandler: url %v", url)

	segs := strings.Split(url, "/")
	if len(segs) != 6 {
		switch r.Method {

		case "GET":
			resp := &BackupResponse{Code: RESP_ERROR, Error: fmt.Sprintf("Malformed url %v", r.URL.Path)}
			send(http.StatusBadRequest, w, resp)

		case "POST":
			resp := &RestoreResponse{Code: RESP_ERROR, Error: fmt.Sprintf("Malformed url %v", r.URL.Path)}
			send(http.StatusBadRequest, w, resp)

		default:
			send(http.StatusBadRequest, w, fmt.Sprintf("Unsupported method %v", r.Method))
		}

		return
	}

	bucket := segs[4]
	function := segs[5]

	switch function {

	case "backup":
		// Note that for backup, bucketReqHandler does not validate input. Input
		// validation is performed in the local RPC implementation on each node.
		// The local RPC handler should not skip the input validation.

		include := r.FormValue("include")
		exclude := r.FormValue("exclude")

		logging.Debugf("bucketReqHandler:backup url %v, include %v, exclude %v", url, include, exclude)

		if !m.authorizeBucketRequest(w, r, creds, bucket, include, exclude) {
			return
		}

		switch r.Method {

		case "GET":
			// Backup
			clusterMeta, err := m.bucketBackupHandler(bucket, include, exclude, r)
			if err == nil {
				resp := &BackupResponse{Code: RESP_SUCCESS, Result: *clusterMeta}
				send(http.StatusOK, w, resp)
			} else {
				logging.Infof("RequestHandler::bucketBackupHandler: err %v", err)
				resp := &BackupResponse{Code: RESP_ERROR, Error: err.Error()}
				send(http.StatusInternalServerError, w, resp)
			}

		case "POST":
			status, errStr := m.bucketRestoreHandler(bucket, include, exclude, r)
			if status == http.StatusOK {
				send(http.StatusOK, w, &RestoreResponse{Code: RESP_SUCCESS})
			} else {
				send(http.StatusInternalServerError, w, &RestoreResponse{Code: RESP_ERROR, Error: errStr})
			}

		default:
			send(http.StatusBadRequest, w, fmt.Sprintf("Unsupported method %v", r.Method))
		}

	default:
		send(http.StatusBadRequest, w, fmt.Sprintf("Malformed URL %v", r.URL.Path))
	}
}

func host2file(hostname string) string {

	hostname = strings.Replace(hostname, ".", "_", -1)
	hostname = strings.Replace(hostname, ":", "_", -1)

	return hostname
}

//
// Handler for /api/v1/bucket/<bucket-name>/<function-name>
//
func BucketRequestHandler(w http.ResponseWriter, r *http.Request, creds cbauth.Creds) {
	handlerContext.bucketReqHandler(w, r, creds)
}

//
// Schedule tokens
//
var SCHED_TOKEN_CHECK_INTERVAL = 5000 // Milliseconds

type schedTokenMonitor struct {
	indexes   []*IndexStatus
	listener  *mc.CommandListener
	lock      sync.Mutex
	lCloseCh  chan bool
	processed map[string]common.IndexerId

	cinfo *common.ClusterInfoCache
	mgr   *IndexManager
}

func newSchedTokenMonitor(mgr *IndexManager) *schedTokenMonitor {

	lCloseCh := make(chan bool)
	listener := mc.NewCommandListener(lCloseCh, false, false, false, false, true, true)

	s := &schedTokenMonitor{
		indexes:   make([]*IndexStatus, 0),
		listener:  listener,
		lCloseCh:  lCloseCh,
		processed: make(map[string]common.IndexerId),
		mgr:       mgr,
	}

	s.listener.ListenTokens()

	cinfo := s.mgr.reqcic.GetClusterInfoCache()
	if cinfo == nil {
		logging.Fatalf("newSchedTokenMonitor: ClusterInfoCache unavailable")
		return s
	}

	s.cinfo = cinfo
	return s
}

func (s *schedTokenMonitor) getNodeAddr(token *mc.ScheduleCreateToken) (string, error) {
	if s.cinfo == nil {
		s.cinfo = s.mgr.reqcic.GetClusterInfoCache()
		if s.cinfo == nil {
			return "", fmt.Errorf("ClusterInfoCache unavailable")
		}
	}

	nodeUUID := fmt.Sprintf("%v", token.IndexerId)
	nid, found := s.cinfo.GetNodeIdByUUID(nodeUUID)
	if !found {
		return "", fmt.Errorf("node id for %v not found", nodeUUID)
	}

	return s.cinfo.GetServiceAddress(nid, "mgmt")
}

func (s *schedTokenMonitor) makeIndexStatus(token *mc.ScheduleCreateToken) *IndexStatus {

	mgmtAddr, err := s.getNodeAddr(token)
	if err != nil {
		logging.Errorf("schedTokenMonitor:makeIndexStatus error in getNodeAddr: %v", err)
		return nil
	}

	defn := &token.Definition
	numPartitons := defn.NumPartitions
	stmt := common.IndexStatement(*defn, int(numPartitons), -1, true)

	// TODO: Scheduled: Should we rename it to ScheduledBuild ?

	// Use DefnId for InstId as a placeholder value because InstId cannot zero.
	return &IndexStatus{
		DefnId:       defn.DefnId,
		InstId:       common.IndexInstId(defn.DefnId),
		Name:         defn.Name,
		Bucket:       defn.Bucket,
		Scope:        defn.Scope,
		Collection:   defn.Collection,
		IsPrimary:    defn.IsPrimary,
		SecExprs:     defn.SecExprs,
		WhereExpr:    defn.WhereExpr,
		IndexType:    common.GetStorageMode().String(),
		Status:       "Scheduled for Creation",
		Definition:   stmt,
		Completion:   0,
		Progress:     0,
		Scheduled:    false,
		Partitioned:  common.IsPartitioned(defn.PartitionScheme),
		NumPartition: int(numPartitons),
		PartitionMap: nil,
		NumReplica:   defn.GetNumReplica(),
		IndexName:    defn.Name,
		LastScanTime: "NA",
		Error:        "",
		Hosts:        []string{mgmtAddr},
	}
}

func (s *schedTokenMonitor) checkProcessed(key string, token *mc.ScheduleCreateToken) (bool, bool) {

	if indexerId, ok := s.processed[key]; ok {
		if token == nil {
			return true, false
		}

		if indexerId == token.IndexerId {
			return true, true
		}

		return true, false
	}

	return false, false
}

func (s *schedTokenMonitor) markProcessed(key string, indexerId common.IndexerId) {
	s.processed[key] = indexerId
}

func (s *schedTokenMonitor) getIndexesFromTokens(createTokens map[string]*mc.ScheduleCreateToken,
	stopTokens map[string]*mc.StopScheduleCreateToken) []*IndexStatus {

	indexes := make([]*IndexStatus, 0, len(createTokens))

	for key, token := range createTokens {
		if marked, match := s.checkProcessed(key, token); marked && match {
			continue
		} else if marked && !match {
			s.updateIndex(token)
			continue
		}

		stopKey := mc.GetStopScheduleCreateTokenPathFromDefnId(token.Definition.DefnId)
		if _, ok := stopTokens[stopKey]; ok {
			continue
		}

		// TODO: Check for the index in s.indexes, before checking for stop token.

		// Explicitly check for stop token.
		stopToken, err := mc.GetStopScheduleCreateToken(token.Definition.DefnId)
		if err != nil {
			logging.Errorf("schedTokenMonitor:getIndexesFromTokens error (%v) in getting stop schedule create token for %v",
				err, token.Definition.DefnId)
			continue
		}

		if stopToken != nil {
			logging.Debugf("schedTokenMonitor:getIndexesFromTokens stop schedule token exists for %v",
				token.Definition.DefnId)
			if marked, _ := s.checkProcessed(key, token); marked {
				marked := s.markIndexFailed(stopToken)
				if marked {
					continue
				} else {
					// This is unexpected as checkProcessed for this key true.
					// Which means the index should have been found in the s.indexrs.
					logging.Warnf("schedTokenMonitor:getIndexesFromTokens failed to mark index as failed for %v",
						token.Definition.DefnId)
				}
			}

			continue
		}

		idx := s.makeIndexStatus(token)
		if idx == nil {
			continue
		}

		indexes = append(indexes, idx)
		s.markProcessed(key, token.IndexerId)
	}

	for key, token := range stopTokens {
		// If create token was already processed, then just mark the
		// index as failed.
		marked := s.markIndexFailed(token)
		if marked {
			s.markProcessed(key, common.IndexerId(""))
			continue
		}

		scheduleKey := mc.GetScheduleCreateTokenPathFromDefnId(token.DefnId)
		ct, ok := createTokens[scheduleKey]
		if !ok {
			continue
		}

		if marked, _ := s.checkProcessed(key, nil); marked {
			continue
		}

		idx := s.makeIndexStatus(ct)
		if idx == nil {
			continue
		}

		idx.Status = "Error"
		idx.Error = token.Reason

		indexes = append(indexes, idx)
		s.markProcessed(key, common.IndexerId(""))
	}

	return indexes
}

func (s *schedTokenMonitor) markIndexFailed(token *mc.StopScheduleCreateToken) bool {
	// Note that this is an idempotent operation - as long as the value
	// of the token doesn't change.
	for _, index := range s.indexes {
		if index.DefnId == token.DefnId {
			index.Status = "Error"
			index.Error = token.Reason
			return true
		}
	}

	return false
}

func (s *schedTokenMonitor) updateIndex(token *mc.ScheduleCreateToken) {
	for _, index := range s.indexes {
		if index.DefnId == token.Definition.DefnId {
			mgmtAddr, err := s.getNodeAddr(token)
			if err != nil {
				logging.Errorf("schedTokenMonitor:updateIndex error in getNodeAddr: %v", err)
				return
			}
			index.Hosts = []string{mgmtAddr}
			return
		}
	}

	logging.Warnf("schedTokenMonitor:getIndexesFromTokens failed to update index for %v",
		token.Definition.DefnId)

	return
}

func (s *schedTokenMonitor) clenseIndexes(indexes []*IndexStatus,
	stopTokens map[string]*mc.StopScheduleCreateToken, delPaths map[string]bool) []*IndexStatus {

	newIndexes := make([]*IndexStatus, 0, len(indexes))
	for _, idx := range indexes {
		path := mc.GetScheduleCreateTokenPathFromDefnId(idx.DefnId)

		if _, ok := delPaths[path]; ok {
			continue
		}

		path = mc.GetStopScheduleCreateTokenPathFromDefnId(idx.DefnId)
		if _, ok := stopTokens[path]; ok {
			if idx.Status != "Error" {
				continue
			} else {
				newIndexes = append(newIndexes, idx)
			}
		} else {
			newIndexes = append(newIndexes, idx)
		}
	}

	return newIndexes
}

func (s *schedTokenMonitor) getIndexes() []*IndexStatus {
	s.lock.Lock()
	defer s.lock.Unlock()

	createTokens := s.listener.GetNewScheduleCreateTokens()
	stopTokens := s.listener.GetNewStopScheduleCreateTokens()
	delPaths := s.listener.GetDeletedScheduleCreateTokenPaths()

	indexes := s.getIndexesFromTokens(createTokens, stopTokens)

	indexes = append(indexes, s.indexes...)
	s.indexes = indexes
	s.indexes = s.clenseIndexes(s.indexes, stopTokens, delPaths)

	return s.indexes
}

func (s *schedTokenMonitor) Close() {
	s.listener.Close()
}
