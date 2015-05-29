package elasticsearch

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/johann8384/libbeat/common"
	"github.com/johann8384/libbeat/logp"
	"github.com/johann8384/libbeat/outputs"
)

type ElasticsearchOutput struct {
	Index          string
	TopologyExpire int
	Conn           *Elasticsearch
	FlushInterval  time.Duration
	BulkMaxSize    int

	TopologyMap  map[string]string
	sendingQueue chan BulkMsg
}

type PublishedTopology struct {
	Name string
	IPs  string
}

// Initialize Elasticsearch as output
func (out *ElasticsearchOutput) Init(config outputs.MothershipConfig, topology_expire int) error {

	if len(config.Protocol) == 0 {
		config.Protocol = "http"
	}

	url := fmt.Sprintf("%s://%s:%d%s", config.Protocol, config.Host, config.Port, config.Path)

	con := NewElasticsearch(url, config.Username, config.Password)
	out.Conn = con

	if config.Index != "" {
		out.Index = config.Index
	} else {
		out.Index = "packetbeat"
	}

	out.TopologyExpire = 15000
	if topology_expire != 0 {
		out.TopologyExpire = topology_expire /*sec*/ * 1000 // millisec
	}

	out.FlushInterval = 1000 * time.Millisecond
	if config.Flush_interval != nil {
		out.FlushInterval = time.Duration(*config.Flush_interval) * time.Millisecond
	}
	out.BulkMaxSize = 10000
	if config.Bulk_size != nil {
		out.BulkMaxSize = *config.Bulk_size
	}

	err := out.EnableTTL()
	if err != nil {
		logp.Err("Fail to set _ttl mapping: %s", err)
		return err
	}

	out.sendingQueue = make(chan BulkMsg, 1000)
	go out.SendMessagesGoroutine()

	logp.Info("[ElasticsearchOutput] Using Elasticsearch %s", url)
	logp.Info("[ElasticsearchOutput] Using index pattern [%s-]YYYY.MM.DD", out.Index)
	logp.Info("[ElasticsearchOutput] Topology expires after %ds", out.TopologyExpire/1000)
	if out.FlushInterval > 0 {
		logp.Info("[ElasticsearchOutput] Insert events in batches. Flush interval is %s. Bulk size is %d.", out.FlushInterval, out.BulkMaxSize)
	} else {
		logp.Info("[ElasticsearchOutput] Insert events one by one. This might affect the performance of the shipper.")
	}

	return nil
}

// Enable using ttl as paramters in a server-ip doc type
func (out *ElasticsearchOutput) EnableTTL() error {

	// make sure the .packetbeat-topology index exists
	out.Conn.CreateIndex(".packetbeat-topology")

	setting := map[string]interface{}{
		"server-ip": map[string]interface{}{
			"_ttl": map[string]string{"enabled": "true", "default": "15000"},
		},
	}

	_, err := out.Conn.Index(".packetbeat-topology", "server-ip", "_mapping", nil, setting)
	if err != nil {
		return err
	}
	return nil
}

// Get the name of server using a specific IP
func (out *ElasticsearchOutput) GetNameByIP(ip string) string {
	name, exists := out.TopologyMap[ip]
	if !exists {
		return ""
	}
	return name
}

func (out *ElasticsearchOutput) InsertBulkMessage(bulkChannel chan interface{}) {
	close(bulkChannel)
	go func(channel chan interface{}) {
		_, err := out.Conn.Bulk("", "", nil, channel)
		if err != nil {
			logp.Err("Fail to perform many index operations in a single API call: %s", err)
		}
	}(bulkChannel)
}

func (out *ElasticsearchOutput) SendMessagesGoroutine() {
	flushChannel := make(<-chan time.Time)

	if out.FlushInterval > 0 {
		flushTicker := time.NewTicker(out.FlushInterval)
		flushChannel = flushTicker.C
	}

	bulkChannel := make(chan interface{}, out.BulkMaxSize)

	for {
		select {
		case msg := <-out.sendingQueue:
			index := fmt.Sprintf("%s-%d.%02d.%02d", out.Index, msg.Ts.Year(), msg.Ts.Month(), msg.Ts.Day())
			if out.FlushInterval > 0 {
				logp.Debug("output_elasticsearch", "Insert bulk messages in channel of size %d.", len(bulkChannel))
				if len(bulkChannel)+2 > out.BulkMaxSize {
					logp.Debug("output_elasticsearch", "Channel size reached. Calling bulk")
					out.InsertBulkMessage(bulkChannel)
					bulkChannel = make(chan interface{}, out.BulkMaxSize)
				}
				bulkChannel <- map[string]interface{}{
					"index": map[string]interface{}{
						"_index": index,
						"_type":  msg.Event["type"].(string),
					},
				}
				bulkChannel <- msg.Event
			} else {
				logp.Debug("output_elasticsearch", "Insert a single event")
				_, err := out.Conn.Index(index, msg.Event["type"].(string), "", nil, msg.Event)
				if err != nil {
					logp.Err("Fail to index or update: %s", err)
				}
			}
		case _ = <-flushChannel:
			out.InsertBulkMessage(bulkChannel)
			bulkChannel = make(chan interface{}, out.BulkMaxSize)
		}
	}
}

// Each shipper publishes a list of IPs together with its name to Elasticsearch
func (out *ElasticsearchOutput) PublishIPs(name string, localAddrs []string) error {
	logp.Debug("output_elasticsearch", "Publish IPs %s with expiration time %d", localAddrs, out.TopologyExpire)
	params := map[string]string{
		"ttl":     fmt.Sprintf("%d", out.TopologyExpire),
		"refresh": "true",
	}
	_, err := out.Conn.Index(
		".packetbeat-topology", /*index*/
		"server-ip",            /*type*/
		name,                   /* id */
		params,                 /* parameters */
		PublishedTopology{name, strings.Join(localAddrs, ",")} /* body */)

	if err != nil {
		logp.Err("Fail to publish IP addresses: %s", err)
		return err
	}

	out.UpdateLocalTopologyMap()

	return nil
}

// Update local topology map
func (out *ElasticsearchOutput) UpdateLocalTopologyMap() {

	// get all shippers IPs from Elasticsearch
	TopologyMapTmp := make(map[string]string)

	res, err := out.Conn.SearchUri(".packetbeat-topology", "server-ip", nil)
	if err == nil {
		for _, obj := range res.Hits.Hits {
			var result QueryResult
			err = json.Unmarshal(obj, &result)
			if err != nil {
				return
			}

			var pub PublishedTopology
			err = json.Unmarshal(result.Source, &pub)
			if err != nil {
				logp.Err("json.Unmarshal fails with: %s", err)
			}
			// add mapping
			ipaddrs := strings.Split(pub.IPs, ",")
			for _, addr := range ipaddrs {
				TopologyMapTmp[addr] = pub.Name
			}
		}
	} else {
		logp.Err("Getting topology map fails with: %s", err)
	}

	// update topology map
	out.TopologyMap = TopologyMapTmp

	logp.Debug("output_elasticsearch", "Topology map %s", out.TopologyMap)
}

// Publish an event
func (out *ElasticsearchOutput) PublishEvent(ts time.Time, event common.MapStr) error {

	out.sendingQueue <- BulkMsg{Ts: ts, Event: event}

	//_, err := out.Conn.Index(index, event["type"].(string), "", nil, event)
	logp.Debug("output_elasticsearch", "Publish event: %s", event)
	return nil
}
