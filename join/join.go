package join

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/etcd/Godeps/_workspace/src/golang.org/x/net/context"
	"github.com/coreos/etcd/client"
	"github.com/coreos/etcd/rafthttp"
	"github.com/coreos/etcd/store"
	"github.com/davecgh/go-spew/spew"
	"github.com/golang/glog"
	"github.com/sttts/elastic-etcd/node"
	"golang.org/x/net/context/ctxhttp"
)

type Strategy string

const (
	LivenessTimeout  = time.Second * 5
	EtcdTimeout      = time.Second * 5
	DiscoveryTimeout = time.Second * 10

	PreparedStrategy = Strategy("prepared")
	PruneStrategy    = Strategy("prune")
	ReplaceStrategy  = Strategy("replace")
	AddStrategy      = Strategy("add")

	maxUint = ^uint(0)
	maxInt  = int(maxUint >> 1)
)

type Result struct {
	InitialCluster      []string
	InitialClusterState string
	AdvertisePeerUrls   string
	Discovery           string
	Name                string
}

func alive(ctx context.Context, m client.Member) bool {
	ctx, _ = context.WithTimeout(ctx, LivenessTimeout)
	glog.V(6).Infof("Testing liveness of %s=%v", m.Name, m.PeerURLs)
	for _, u := range m.PeerURLs {
		resp, err := ctxhttp.Get(ctx, http.DefaultClient, u+rafthttp.ProbingPrefix)
		if err == nil && resp.StatusCode == http.StatusOK {
			return true
		}
	}

	return false
}

func active(ctx context.Context, m client.Member) (bool, error) {
	ctx, _ = context.WithTimeout(ctx, EtcdTimeout)

	c, err := client.New(client.Config{
		Endpoints:               m.ClientURLs,
		Transport:               client.DefaultTransport,
		HeaderTimeoutPerRequest: 5 * time.Second,
	})
	if err != nil {
		return false, err
	}
	mapi := client.NewMembersAPI(c)
	glog.V(6).Infof("Testing whether %s=%v knows the leader", m.Name, m.PeerURLs)
	leader, err := mapi.Leader(ctx)
	if err != nil {
		return false, err
	}
	return leader != nil, nil
}

func clusterExistingHeuristic(
	ctx context.Context,
	size int, nodes []node.DiscoveryNode,
) ([]node.DiscoveryNode, error) {
	quorum := size/2 + 1

	if nodes == nil {
		glog.V(4).Infof("No nodes found in discovery service. Assuming new cluster.")
		return nil, nil
	}

	wg := sync.WaitGroup{}
	wg.Add(len(nodes))
	activeNodes := make([]node.DiscoveryNode, 0, len(nodes))
	for _, n := range nodes {
		go func(n node.DiscoveryNode) {
			defer wg.Done()
			if !alive(ctx, n.Member) {
				glog.Infof("Node %s looks dead", n.NamedPeerUrls())
				return
			}
			if ok, err := active(ctx, n.Member); !ok {
				if err != nil {
					glog.Error(err)
				}
				glog.Infof("Node %s is not in a healthy cluster.", n.NamedPeerUrls())
				return
			}
			glog.Infof("Node %s looks alive and active in a cluster", n.NamedPeerUrls())
			activeNodes = append(activeNodes, n)
		}(n)
	}
	wg.Wait()

	if len(nodes) < quorum {
		glog.V(4).Infof(
			"Only %d nodes found in discovery service, less than a quorum of %d. Assuming new cluster.",
			len(nodes),
			quorum,
		)
		return nil, nil
	}

	if len(nodes) == size {
		glog.V(4).Infof("Cluster is full. Assuming existing cluster.")
		return activeNodes, nil
	}

	if len(activeNodes) > 0 {
		return activeNodes, nil
	}

	return nil, nil
}

func discoveryValue(ctx context.Context, baseUrl, key string) (*store.Event, error) {
	ctx, _ = context.WithTimeout(ctx, DiscoveryTimeout)

	url := baseUrl + key
	glog.V(6).Infof("Getting %s", url)
	resp, err := ctxhttp.Get(ctx, http.DefaultClient, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := ioutil.ReadAll(resp.Body)
		return nil, fmt.Errorf("status code %d from %q: %s", resp.StatusCode, url, body)
	}

	var res store.Event
	err = json.NewDecoder(resp.Body).Decode(&res)
	if err != nil {
		return nil, fmt.Errorf("invalid answer from %q: %v", url, err)
	}

	glog.V(9).Infof("Got: %s", spew.Sdump(res))

	return &res, nil
}

func deleteDiscoveryMachine(ctx context.Context, baseUrl, id string) (bool, error) {
	ctx, _ = context.WithTimeout(ctx, DiscoveryTimeout)

	url := baseUrl + "/" + strings.TrimLeft(id, "/")
	req, err := http.NewRequest("DELETE", url, strings.NewReader(""))
	if err != nil {
		return false, err
	}
	resp, err := ctxhttp.Do(ctx, http.DefaultClient, req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := ioutil.ReadAll(resp.Body)
		return false, fmt.Errorf("status code %d on DELETE for %q: %s", resp.StatusCode, url, body)
	}

	return true, nil
}

func Join(
	discoveryUrl, name, initialAdvertisePeerUrls string,
	fresh bool,
	clientPort, clusterSize int,
	strategy Strategy,
) (*Result, error) {
	ctx := context.Background()

	res, err := discoveryValue(ctx, discoveryUrl, "/")
	if err != nil {
		return nil, err
	}
	nodes := make([]node.DiscoveryNode, 0, len(res.Node.Nodes))
	for _, nn := range res.Node.Nodes {
		if nn.Value == nil {
			glog.V(5).Infof("Skipping %q because no value exists", nn.Key)
		}
		n, err := node.NewDiscoveryNode(*nn.Value, clientPort)
		if err != nil {
			glog.Warningf("invalid peer url %q in discovery service: %v", *nn.Value, err)
			continue
		}
		nodes = append(nodes, *n)
	}

	if clusterSize < 0 {
		res, err = discoveryValue(ctx, discoveryUrl, "/_config/size")
		if err != nil {
			return nil, fmt.Errorf("cannot get discovery url cluster size: %v", err)
		}

		size, _ := strconv.ParseInt(*res.Node.Value, 10, 16)
		clusterSize = int(size)

		glog.V(2).Infof("Got a target cluster size of %d from the discovery url", clusterSize)
	} else if clusterSize == 0 {
		clusterSize = maxInt
	}

	activeNodes, err := clusterExistingHeuristic(ctx, clusterSize, nodes)
	if err != nil {
		return nil, err
	}
	if activeNodes != nil && len(activeNodes) == 0 {
		// cluster down. Restarting nodes with the same config.

		if fresh {
			return nil, errors.New("Cluster is down. A new node cannot join now.")
		}

		return &Result{
			InitialClusterState: "existing",
			AdvertisePeerUrls:   initialAdvertisePeerUrls,
			Name:                name,
		}, nil
	} else if activeNodes != nil {
		activeNamedUrls := make([]string, 0, len(nodes))
		for _, n := range activeNodes {
			activeNamedUrls = append(activeNamedUrls, n.NamedPeerUrls()...)
		}

		advertisedUrls := strings.Split(initialAdvertisePeerUrls, ",")

		advertisedNamedUrls := make([]string, 0, len(initialAdvertisePeerUrls))
		for _, u := range advertisedUrls {
			advertisedNamedUrls = append(advertisedNamedUrls, fmt.Sprintf("%s=%s", name, u))
		}

		initialNamedUrls := []string{advertisedNamedUrls[0]}
		if strategy != PreparedStrategy && fresh {
			adder, err := NewMemberAdder(
				activeNodes,
				strategy,
				clientPort,
				clusterSize,
				discoveryUrl,
			)
			if err != nil {
				return nil, err
			}
			initialUrls, err := adder.Add(ctx, name, advertisedUrls)
			if err != nil {
				return nil, fmt.Errorf("unable to add node %q with peer urls %q to the cluster: %v", name, initialAdvertisePeerUrls, err)
			}

			initialNamedUrls = []string{}
			for _, u := range initialUrls {
				initialNamedUrls = append(initialNamedUrls, fmt.Sprintf("%s=%s", name, u))
			}
		}

		return &Result{
			InitialCluster:      append(initialNamedUrls, activeNamedUrls...),
			InitialClusterState: "existing",
			AdvertisePeerUrls:   initialAdvertisePeerUrls,
			Name:                name,
		}, nil
	} else {
		return &Result{
			InitialClusterState: "new",
			Discovery:           discoveryUrl,
			AdvertisePeerUrls:   initialAdvertisePeerUrls,
			Name:                name,
		}, nil
	}
}
