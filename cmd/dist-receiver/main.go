package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	b64 "encoding/base64"

	icore "github.com/ipfs/boxo/coreiface"
	icorepath "github.com/ipfs/boxo/coreiface/path"
	"github.com/ipfs/boxo/files"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"

	"github.com/ipfs/kubo/config"
	"github.com/ipfs/kubo/core"
	"github.com/ipfs/kubo/core/coreapi"
	"github.com/ipfs/kubo/core/node/libp2p"
	"github.com/ipfs/kubo/plugin/loader"
	"github.com/ipfs/kubo/repo/fsrepo"
)

var stageNow int
var sig chan bool
var targets []string

func main() {
	args := os.Args
	// 1: bootstrap node
	// 2: cid
	// 3: file name
	// 4: target directory

	stageNow = 0
	sig = make(chan bool)

	http.HandleFunc("/stage", stage)
	http.HandleFunc("/next", next)
	http.HandleFunc("/connect", connect)
	go func() {
		http.ListenAndServe("0.0.0.0:4002", nil)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	node, _, err := spawnEphemeral(ctx, args[1])
	if err != nil {
		panic(fmt.Errorf("failed to spawn ephemeral node: %s", err))
	}

	connectToPeers(ctx, node, []string{args[1]})

	stageNow = 1

	<-sig

	connectToPeers(ctx, node, targets)

	cid := icorepath.New(args[2])

	node.Dht().Provide(ctx, cid)
	node.Pin().Add(ctx, cid)

	rootNode, err := node.Unixfs().Get(ctx, cid)

	if err != nil {
		panic(fmt.Errorf("could not get file with CID: %s", err))
	}

	os.RemoveAll(args[3])

	err = files.WriteTo(rootNode, args[3])
	if err != nil {
		panic(fmt.Errorf("could not write out the fetched CID: %s", err))
	}

	err = os.MkdirAll(args[4], os.ModePerm)
	if err != nil {
		panic(fmt.Errorf("failed to create target directory: %s", err))
	}

	cmd := exec.Command("tar", "-C", args[4], "-xzf", args[3])
	_, err = cmd.Output()
	if err != nil {
		panic(fmt.Errorf("failed to uncompress resource file: %s", err))
	}

	stageNow = 2

	<-sig

	exec.Command("rm", args[3])
}

func createNode(ctx context.Context, repoPath string) (*core.IpfsNode, error) {
	// Open the repo
	repo, err := fsrepo.Open(repoPath)
	if err != nil {
		return nil, err
	}

	// Construct the node
	nodeOptions := &core.BuildCfg{
		Online:  true,
		Routing: libp2p.DHTOption, // This option sets the node to be a full DHT node (both fetching and storing DHT Records)
		// Routing: libp2p.DHTClientOption, // This option sets the node to be a client DHT node (only fetching records)
		Repo: repo,
	}

	return core.NewNode(ctx, nodeOptions)
}

func spawnEphemeral(ctx context.Context, bootstrap string) (icore.CoreAPI, *core.IpfsNode, error) {
	err := setupPlugins("")
	if err != nil {
		return nil, nil, err
	}

	repoPath, err := createTempRepo(bootstrap)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create temp repo: %s", err)
	}

	node, err := createNode(ctx, repoPath)
	if err != nil {
		return nil, nil, err
	}

	api, err := coreapi.NewCoreAPI(node)

	return api, node, err
}

func setupPlugins(externalPluginsPath string) error {
	plugins, err := loader.NewPluginLoader(filepath.Join(externalPluginsPath, "plugins"))
	if err != nil {
		return fmt.Errorf("error loading plugins: %s", err)
	}

	if err := plugins.Initialize(); err != nil {
		return fmt.Errorf("error initializing plugins: %s", err)
	}

	if err := plugins.Inject(); err != nil {
		return fmt.Errorf("error initializing plugins: %s", err)
	}

	return nil
}

func createTempRepo(bootstrap string) (string, error) {
	repoPath, err := os.MkdirTemp("", "ipfs-shell")
	if err != nil {
		return "", fmt.Errorf("failed to get temp dir: %s", err)
	}

	cfg, err := config.Init(io.Discard, 2048)
	if err != nil {
		return "", err
	}

	var bs []string
	cfg.Bootstrap = bs

	err = fsrepo.Init(repoPath, cfg)
	if err != nil {
		return "", fmt.Errorf("failed to init ephemeral node: %s", err)
	}

	return repoPath, nil
}

func connectToPeers(ctx context.Context, ipfs icore.CoreAPI, peers []string) error {
	var wg sync.WaitGroup
	peerInfos := make(map[peer.ID]*peer.AddrInfo, len(peers))
	for _, addrStr := range peers {
		addr, err := ma.NewMultiaddr(addrStr)
		if err != nil {
			return err
		}
		pii, err := peer.AddrInfoFromP2pAddr(addr)
		if err != nil {
			return err
		}
		pi, ok := peerInfos[pii.ID]
		if !ok {
			pi = &peer.AddrInfo{ID: pii.ID}
			peerInfos[pi.ID] = pi
		}
		pi.Addrs = append(pi.Addrs, pii.Addrs...)
	}

	wg.Add(len(peerInfos))
	for _, peerInfo := range peerInfos {
		go func(peerInfo *peer.AddrInfo) {
			defer wg.Done()
			err := ipfs.Swarm().Connect(ctx, *peerInfo)
			if err != nil {
				log.Printf("failed to connect to %s: %s", peerInfo.ID, err)
			}
		}(peerInfo)
	}
	wg.Wait()
	return nil
}

func stage(w http.ResponseWriter, req *http.Request) {
	fmt.Fprintf(w, "%d", stageNow)
}

func next(w http.ResponseWriter, req *http.Request) {
	sig <- true
}

func connect(w http.ResponseWriter, req *http.Request) {
	target := req.URL.Query().Get("target")
	targetDecoded, err := b64.StdEncoding.DecodeString(target)
	if err != nil {
		return
	}

	target = string(targetDecoded)

	targets = strings.Split(target, ",")
}
