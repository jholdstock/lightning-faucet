package main

import (
	"encoding/hex"
	"fmt"
	"html/template"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	macaroon "gopkg.in/macaroon.v2"

	"github.com/davecgh/go-spew/spew"
	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/dcrutil"
	"github.com/decred/dcrd/wire"
	"github.com/decred/dcrlnd/lnrpc"
	"github.com/decred/dcrlnd/macaroons"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

const (
	// maxChannelSize is the larget channel that the faucet will create to
	// another peer.
	maxChannelSize int64 = (1 << 30)

	// minChannelSize is the smallest channel that the faucet will extend
	// to a peer.
	minChannelSize int64 = 50000
)

var (
	lndHomeDir             = dcrutil.AppDataDir("dcrlnd", false)
	defaultTLSCertFilename = "tls.cert"
	tlsCertPath            = filepath.Join(lndHomeDir, defaultTLSCertFilename)

	defaultMacaroonFilename = "admin.macaroon"
	defaultMacaroonPath     = filepath.Join(
		lndHomeDir, "data", "chain", "decred", "testnet",
		defaultMacaroonFilename,
	)

	lndFaucetHomeDir   = dcrutil.AppDataDir("dcrlnfaucet", false)
	defaultLogFilename = "dcrlnfaucet.log"
	defaultLogPath     = filepath.Join(
		lndFaucetHomeDir, "logs", "decred", "testnet",
		defaultLogFilename,
	)
	defaultLogLevel = "info"
)

// chanCreationError is an enum which describes the exact nature of an error
// encountered when a user attempts to create a channel with the faucet. This
// enum is used within the templates to determine at which input item the error
// occurred  and also to generate an error string unique to the error.
type chanCreationError uint8

const (
	// NoError is the default error which indicates either the form hasn't
	// yet been submitted or no errors have arisen.
	NoError chanCreationError = iota

	// InvalidAddress indicates that the passed node address is invalid.
	InvalidAddress

	// NotConnected indicates that the target peer isn't connected to the
	// faucet.
	NotConnected

	// ChanAmountNotNumber indicates that the amount specified for the
	// amount to fund the channel with isn't actually a number.
	ChanAmountNotNumber

	// ChannelTooLarge indicates that the amounts specified to fund the
	// channel with is greater than maxChannelSize.
	ChannelTooLarge

	// ChannelTooSmall indicates that the channel size required is below
	// minChannelSize.
	ChannelTooSmall

	// PushIncorrect indicates that the amount specified to push to the
	// other end of the channel is greater-than-or-equal-to the local
	// funding amount.
	PushIncorrect

	// ChannelOpenFail indicates some error occurred when attempting to
	// open a channel with the target peer.
	ChannelOpenFail

	// HaveChannel indicates that the faucet already has a channel open
	// with the target node.
	HaveChannel

	// HavePendingChannel indicates that the faucet already has a channel
	// pending with the target node.
	HavePendingChannel
)

// String returns a human readable string describing the chanCreationError.
// This string is used in the templates in order to display the error to the
// user.
func (c chanCreationError) String() string {
	switch c {
	case NoError:
		return ""
	case InvalidAddress:
		return "Not a valid public key"
	case NotConnected:
		return "Faucet cannot connect to this node"
	case ChanAmountNotNumber:
		return "Amount must be a number"
	case ChannelTooLarge:
		return "Amount is too large"
	case ChannelTooSmall:
		return fmt.Sprintf("Minimum channel size is is %d DCR", minChannelSize)
	case PushIncorrect:
		return "Initial Balance is incorrect"
	case ChannelOpenFail:
		return "Faucet is not able to open a channel with this node"
	case HaveChannel:
		return "Faucet already has an active channel with this node"
	case HavePendingChannel:
		return "Faucet already has a pending channel with this node"
	default:
		return fmt.Sprintf("%v", uint8(c))
	}
}

// lightningFaucet is a Decred Channel Faucet. The faucet itself is a web app
// that is capable of programmatically opening channels with users with the
// size of the channel parametrized by the user. The faucet required a
// connection to a local lnd node in order to operate properly. The faucet
// implements the constrains on the channel size, and also will only open a
// single channel to a particular node. Finally, the faucet will periodically
// close channels based on their age as the faucet will only open up 100
// channels total at any given time.
type lightningFaucet struct {
	lnd lnrpc.LightningClient

	templates *template.Template

	network string

	openChanMtx  sync.RWMutex
	openChannels map[wire.OutPoint]time.Time
}

// newLightningFaucet creates a new channel faucet that's bound to a cluster of
// lnd nodes, and uses the passed templates to render the web page.
func newLightningFaucet(lndHost string,
	templates *template.Template, network string) (*lightningFaucet, error) {

	// First attempt to establish a connection to lnd's RPC sever.
	creds, err := credentials.NewClientTLSFromFile(tlsCertPath, "")
	if err != nil {
		return nil, fmt.Errorf("unable to read cert file: %v", err)
	}
	opts := []grpc.DialOption{grpc.WithTransportCredentials(creds)}

	// Load the specified macaroon file.
	macPath := cleanAndExpandPath(defaultMacaroonPath)
	macBytes, err := ioutil.ReadFile(macPath)
	if err != nil {
		return nil, err
	}
	mac := &macaroon.Macaroon{}
	if err = mac.UnmarshalBinary(macBytes); err != nil {
		return nil, err
	}

	// Now we append the macaroon credentials to the dial options.
	opts = append(
		opts,
		grpc.WithPerRPCCredentials(macaroons.NewMacaroonCredential(mac)),
	)

	conn, err := grpc.Dial(*lndNodes, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to dial to lnd's gRPC server: %v", err)
	}

	// If we're able to connect out to the lnd node, then we can start up
	// the faucet safely.
	lnd := lnrpc.NewLightningClient(conn)

	return &lightningFaucet{
		lnd:       lnd,
		templates: templates,
		network:   network,
	}, nil
}

// Start launches all the goroutines necessary for routine operation of the
// lightning faucet.
func (l *lightningFaucet) Start() {
	go l.zombieChanSweeper()
}

// zombieChanSweeper is a goroutine that is tasked with cleaning up "zombie"
// channels. A zombie channel is a channel in which the peer we have the
// channel open with hasn't been online for greater than 48 hours. We'll
// periodically perform a sweep every hour to close out any lingering zombie
// channels.
//
// NOTE: This MUST be run as a goroutine.
func (l *lightningFaucet) zombieChanSweeper() {
	log.Info("zombie chan sweeper active")

	// Any channel peer that hasn't been online in more than 48 hours past
	// from now will have their channels closed out.
	timeCutOff := time.Now().Add(-time.Hour * 48)

	// Upon initial boot, we'll do a scan to close out any channels that
	// are now considered zombies while we were down.
	l.sweepZombieChans(timeCutOff)

	// Every hour we'll consume a new tick and perform a sweep to close out
	// any zombies channels.
	zombieTicker := time.NewTicker(time.Hour * 1)
	for _ = range zombieTicker.C {
		log.Info("Performing zombie channel sweep!")

		// In order to ensure we close out the proper channels, we also
		// calculate the 48 hour offset from the point of our next
		// tick.
		timeCutOff = time.Now().Add(-time.Hour * 48)

		// With the time cut off calculated, we'll force close any
		// channels that are now considered "zombies".
		l.sweepZombieChans(timeCutOff)
	}
}

// strPointToChanPoint concerts a string outpoint (txid:index) into an lnrpc
// ChannelPoint object.
func strPointToChanPoint(stringPoint string) (*lnrpc.ChannelPoint, error) {
	s := strings.Split(stringPoint, ":")

	txid, err := chainhash.NewHashFromStr(s[0])
	if err != nil {
		return nil, err
	}

	index, err := strconv.Atoi(s[1])
	if err != nil {
		return nil, err
	}

	return &lnrpc.ChannelPoint{
		FundingTxid: &lnrpc.ChannelPoint_FundingTxidBytes{
			txid[:],
		},
		OutputIndex: uint32(index),
	}, nil
}

// sweepZombieChans performs a sweep of the set of channels that the faucet has
// active to close out any channels that are now considered to be a "zombie". A
// channel is a zombie if the peer with have the channel open is currently
// offline, and we haven't detected them as being online since timeCutOff.
//
// TODO(roasbeef): after removing the node ANN on startup, will need to rely on
// LinkNode information.
func (l *lightningFaucet) sweepZombieChans(timeCutOff time.Time) {
	// Fetch all the facuet's currently open channels.
	openChanReq := &lnrpc.ListChannelsRequest{}
	openChannels, err := l.lnd.ListChannels(ctxb, openChanReq)
	if err != nil {
		log.Errorf("unable to fetch open channels: %v", err)
		return
	}

	for _, channel := range openChannels.Channels {
		// For each channel we'll first fetch the announcement
		// information for the peer that we have the channel open with.
		nodeInfoResp, err := l.lnd.GetNodeInfo(ctxb,
			&lnrpc.NodeInfoRequest{
				PubKey: channel.RemotePubkey,
			})
		if err != nil {
			log.Errorf("unable to get node pubkey: %v", err)
			continue
		}

		// Convert the unix time stamp into a time.Time object.
		lastSeen := time.Unix(int64(nodeInfoResp.Node.LastUpdate), 0)

		// If the last time we saw this peer online was _before_ our
		// time cutoff, and the peer isn't currently online, then we'll
		// force close out the channel.
		if lastSeen.Before(timeCutOff) && !channel.Active {
			log.Infof("ChannelPoint(%v) is a zombie, last seen: %v",
				channel.ChannelPoint, lastSeen)

			chanPoint, err := strPointToChanPoint(channel.ChannelPoint)
			if err != nil {
				log.Errorf("unable to get chan point: %v", err)
				continue
			}
			txid, err := l.closeChannel(chanPoint, true)
			if err != nil {
				log.Errorf("unable to close zombie chan: %v", err)
				continue
			}

			log.Infof("closed zombie chan, txid: %v", txid)
		}
	}
}

// closeChannel closes out a target channel optionally executing a force close.
// This function will block until the closing transaction has been broadcast.
func (l *lightningFaucet) closeChannel(chanPoint *lnrpc.ChannelPoint,
	force bool) (*chainhash.Hash, error) {

	closeReq := &lnrpc.CloseChannelRequest{
		ChannelPoint: chanPoint,
		Force:        force,
	}
	stream, err := l.lnd.CloseChannel(ctxb, closeReq)
	if err != nil {
		log.Errorf("unable to start channel close: %v", err)
	}

	// Consume the first response which'll be sent once the closing
	// transaction has been broadcast.
	resp, err := stream.Recv()
	if err != nil {
		return nil, fmt.Errorf("unable to close chan: %v", err)
	}

	update, ok := resp.Update.(*lnrpc.CloseStatusUpdate_ClosePending)
	if !ok {
		return nil, fmt.Errorf("didn't get a pending update")
	}

	// Convert the raw bytes into a new chainhash so we gain access to its
	// utility methods.
	closingHash := update.ClosePending.Txid
	return chainhash.NewHash(closingHash)
}

// homePageContext defines the initial context required for rendering home
// page. The home page displays some basic statistics, errors in the case of an
// invalid channel submission, and finally a splash page upon successful
// creation of a channel.
type homePageContext struct {
	// NumCoins is the number of coins in BTC that the faucet has available
	// for channel creation.
	NumCoins float64

	// GitCommitHash is the git HEAD's commit hash of
	// $GOPATH/src/github.com/lightningnetwork/lnd
	GitCommitHash string

	// NodeAddr is the full <pubkey>@host:port where the faucet can be
	// connect to.
	NodeAddr string

	// SubmissionError is a enum that stores if any error took place during
	// the creation of a channel.
	SubmissionError chanCreationError

	// ChannelTxid is the txid of the created funding channel. If this
	// field is an empty string, then that indicates the channel hasn't yet
	// been created.
	ChannelTxid string

	// NumConfs is the number of confirmations required for the channel to
	// open up.
	NumConfs uint32

	// Network is the network the faucet is running on.
	Network string

	// FormFields contains the values which were submitted through the form.
	FormFields map[string]string

	// PendingChannels contains all of this faucets pending channels.
	PendingChannels []*lnrpc.PendingChannelsResponse_PendingOpenChannel

	// ActiveChannels contains all of this faucets active channels.
	ActiveChannels []*lnrpc.Channel
}

// fetchHomeState is helper functions that populates the homePageContext with
// the latest state from the local lnd node.
func (l *lightningFaucet) fetchHomeState() (*homePageContext, error) {
	// First query for the general information from the lnd node, this'll
	// be used to populate the number of active channel as well as the
	// identity of the node.
	infoReq := &lnrpc.GetInfoRequest{}
	nodeInfo, err := l.lnd.GetInfo(ctxb, infoReq)
	if err != nil {
		log.Errorf("rpc GetInfoRequest failed: %v", err)
		return nil, err
	}

	activeChanReq := &lnrpc.ListChannelsRequest{}
	activeChannels, err := l.lnd.ListChannels(ctxb, activeChanReq)
	if err != nil {
		log.Errorf("rpc ListChannels failed: %v", err)
		return nil, err
	}

	pendingChanReq := &lnrpc.PendingChannelsRequest{}
	pendingChannels, err := l.lnd.PendingChannels(ctxb, pendingChanReq)
	if err != nil {
		log.Errorf("rpc PendingChannels failed: %v", err)
		return nil, err
	}

	// Next obtain the wallet's available balance which indicates how much
	// we can allocate towards channels.
	balReq := &lnrpc.WalletBalanceRequest{}
	walletBalance, err := l.lnd.WalletBalance(ctxb, balReq)
	if err != nil {
		log.Errorf("rpc WalletBalance failed: %v", err)
		return nil, err
	}

	// Parse the git commit used to build the node, assuming it was built
	// with `make install`.
	gitHash := ""
	if p := strings.LastIndex(nodeInfo.Version, "commit="); p > -1 && len(nodeInfo.Version)-p-7 >= 40 {
		// Read the right-most 40 chars and assume it's the git hash.
		gitHash = nodeInfo.Version[len(nodeInfo.Version)-40:]
	}

	nodeAddr := ""
	if len(nodeInfo.Uris) == 0 {
		log.Warn("nodeInfo did not include a URI. external_ip config of dcrlnd is probably not set")
	} else {
		nodeAddr = nodeInfo.Uris[0]
	}

	return &homePageContext{
		NumCoins:        dcrutil.Amount(walletBalance.ConfirmedBalance).ToCoin(),
		GitCommitHash:   strings.Replace(string(gitHash), "'", "", -1),
		NodeAddr:        nodeAddr,
		NumConfs:        3,
		Network:         l.network,
		FormFields:      make(map[string]string),
		ActiveChannels:  activeChannels.Channels,
		PendingChannels: pendingChannels.PendingOpenChannels,
	}, nil
}

// faucetHome renders the main home page for the faucet. This includes the form
// to create channels, the network statistics, and the splash page upon channel
// success.
//
// NOTE: This method implements the http.Handler interface.
func (l *lightningFaucet) faucetHome(w http.ResponseWriter, r *http.Request) {
	// First obtain the home template from our cache of pre-compiled
	// templates.
	homeTemplate := l.templates.Lookup("index.html")

	// In order to render the home template we'll need the necessary
	// context, so we'll grab that from the lnd daemon now in order to get
	// the most up to date state.
	homeInfo, err := l.fetchHomeState()
	if err != nil {
		log.Error("unable to fetch home state")
		http.Error(w, "unable to render home page", http.StatusInternalServerError)
		return
	}

	// If the method is GET, then we'll render the home page with the form
	// itself.
	switch {
	case r.Method == http.MethodGet:
		homeTemplate.Execute(w, homeInfo)

	// Otherwise, if the method is POST, then the user is submitting the
	// form to open a channel, so we'll pass that off to the openChannel
	// handler.
	case r.Method == http.MethodPost:
		l.openChannel(homeTemplate, homeInfo, w, r)

	// If the method isn't either of those, then this is an error as we
	// only support the two methods above.
	default:
		http.Error(w, "", http.StatusMethodNotAllowed)
	}

	return
}

func (l *lightningFaucet) pendingChannelExistsWithNode(nodePub string) bool {
	pendingChanReq := &lnrpc.PendingChannelsRequest{}
	resp, err := l.lnd.PendingChannels(ctxb, pendingChanReq)
	if err != nil {
		return false
	}

	for _, channel := range resp.PendingOpenChannels {
		if channel.Channel.RemoteNodePub == nodePub {
			return true
		}
	}

	return false
}

// activeCahannelExistsWithNode return true if the faucet already has a channel open
// with the target node, and false otherwise.
func (l *lightningFaucet) channelExistsWithNode(nodePub string) bool {
	listChanReq := &lnrpc.ListChannelsRequest{}
	resp, err := l.lnd.ListChannels(ctxb, listChanReq)
	if err != nil {
		return false
	}

	for _, channel := range resp.Channels {
		if channel.RemotePubkey == nodePub {
			return true
		}
	}

	return false
}

// connectedToNode returns true if the faucet is connected to the node, and
// false otherwise.
func (l *lightningFaucet) connectedToNode(nodePub string) bool {
	peersReq := &lnrpc.ListPeersRequest{}
	resp, err := l.lnd.ListPeers(ctxb, peersReq)
	if err != nil {
		return false
	}

	for _, peer := range resp.Peers {
		if peer.PubKey == nodePub {
			return true
		}
	}

	return false
}

// openChannel is a hybrid http.Handler that handles: the validation of the
// channel creation form, rendering errors to the form, and finally creating
// channels if all the parameters check out.
func (l *lightningFaucet) openChannel(homeTemplate *template.Template,
	homeState *homePageContext, w http.ResponseWriter, r *http.Request) {

	// Before we can obtain the values the user entered in the form, we
	// need to parse all parameters.  First attempt to establish a
	// connection with the
	if err := r.ParseForm(); err != nil {
		http.Error(w, "unable to parse form", 500)
		return
	}

	nodePubStr := r.FormValue("node")
	amt := r.FormValue("amt")
	bal := r.FormValue("bal")

	homeState.FormFields["Node"] = nodePubStr
	homeState.FormFields["Amt"] = amt
	homeState.FormFields["Bal"] = bal

	// With the forms details parsed, extract out the public key of the
	// target peer.
	nodePub, err := hex.DecodeString(nodePubStr)
	if err != nil {
		homeState.SubmissionError = InvalidAddress
		homeTemplate.Execute(w, homeState)
		return
	}

	// If we already have a channel with this peer, then we'll fail the
	// request as we have a policy of only one channel per node.
	if l.channelExistsWithNode(nodePubStr) {
		homeState.SubmissionError = HaveChannel
		homeTemplate.Execute(w, homeState)
		return
	}

	// If we already have a channel with this peer, then we'll fail the
	// request as we have a policy of only one channel per node.
	if l.pendingChannelExistsWithNode(nodePubStr) {
		homeState.SubmissionError = HavePendingChannel
		homeTemplate.Execute(w, homeState)
		return
	}

	// If we're not connected to the node, then we won't be able to extend
	// a channel to them. So we'll exit early with an error here.
	if !l.connectedToNode(nodePubStr) {
		homeState.SubmissionError = NotConnected
		homeTemplate.Execute(w, homeState)
		return
	}

	// With the connection established (or already present) with the target
	// peer, we'll now parse out the rest of the fields, performing
	// validation and exiting early if any field is invalid.
	chanSizeFloat, err := strconv.ParseFloat(amt, 64)
	if err != nil {
		homeState.SubmissionError = ChanAmountNotNumber
		homeTemplate.Execute(w, homeState)
		return
	}
	pushAmtFloat, err := strconv.ParseFloat(bal, 64)
	if err != nil {
		homeState.SubmissionError = PushIncorrect
		homeTemplate.Execute(w, homeState)
		return
	}

	// Convert from input (dcr) to api (atoms) units.
	chanSize := int64(chanSizeFloat * 1e8)
	pushAmt := int64(pushAmtFloat * 1e8)

	// With the initial validation complete, we'll now ensure the channel
	// size and push amt meet our constraints.
	switch {
	// The target channel can't be below the constant min channel size.
	case chanSize < minChannelSize:
		homeState.SubmissionError = ChannelTooSmall
		homeTemplate.Execute(w, homeState)
		return

	// The target channel can't be above the max channel size.
	case chanSize > maxChannelSize:
		homeState.SubmissionError = ChannelTooLarge
		homeTemplate.Execute(w, homeState)
		return

	// The amount pushed to the other side as part of the channel creation
	// MUST be less than the size of the channel itself.
	case pushAmt >= chanSize:
		homeState.SubmissionError = PushIncorrect
		homeTemplate.Execute(w, homeState)
		return
	}

	// If we were able to connect to the peer successfully, and all the
	// parameters check out, then we'll parse out the remaining channel
	// parameters and initiate the funding workflow.
	openChanReq := &lnrpc.OpenChannelRequest{
		NodePubkey:         nodePub,
		LocalFundingAmount: chanSize,
		PushAtoms:          pushAmt,
	}
	log.Infof("attempting to create channel with params: %v",
		spew.Sdump(openChanReq))

	openChanStream, err := l.lnd.OpenChannel(ctxb, openChanReq)
	if err != nil {
		log.Errorf("Opening channel stream failed: %v", err)
		homeState.SubmissionError = ChannelOpenFail
		homeTemplate.Execute(w, homeState)
		return
	}

	// Consume the first update from the open channel stream which
	// indicates that the channel has been broadcast to the network.
	chanUpdate, err := openChanStream.Recv()
	if err != nil {
		log.Errorf("Channel update failed: %v", err)
		homeState.SubmissionError = ChannelOpenFail
		homeTemplate.Execute(w, homeState)
		return
	}

	pendingUpdate := chanUpdate.Update.(*lnrpc.OpenStatusUpdate_ChanPending).ChanPending
	fundingTXID, _ := chainhash.NewHash(pendingUpdate.Txid)

	log.Infof("channel created with txid: %v", fundingTXID)

	homeState.ChannelTxid = fundingTXID.String()
	if err := homeTemplate.Execute(w, homeState); err != nil {
		log.Errorf("unable to render home page: %v", err)
	}
}

// CloseAllChannels attempt unconditionally close ALL of the faucet's currently
// open channels. In the case that a channel is active a cooperative closure
// will be executed, in the case that a channel is inactive, a force close will
// be attempted.
func (l *lightningFaucet) CloseAllChannels() error {
	openChanReq := &lnrpc.ListChannelsRequest{}
	openChannels, err := l.lnd.ListChannels(ctxb, openChanReq)
	if err != nil {
		return fmt.Errorf("unable to fetch open channels: %v", err)
	}

	for _, channel := range openChannels.Channels {
		log.Infof("Attempting to close channel: %s", channel.ChannelPoint)

		chanPoint, err := strPointToChanPoint(channel.ChannelPoint)
		if err != nil {
			log.Errorf("unable to get chan point: %v", err)
			continue
		}

		forceClose := !channel.Active
		if forceClose {
			log.Info("Attempting force close")
		}

		closeTxid, err := l.closeChannel(chanPoint, forceClose)
		if err != nil {
			log.Errorf("unable to close channel: %v", err)
			continue
		}

		log.Infof("closing txid: %v", closeTxid)
	}

	return nil
}

// cleanAndExpandPath expands environment variables and leading ~ in the passed
// path, cleans the result, and returns it.
// This function is taken from https://github.com/btcsuite/btcd
func cleanAndExpandPath(path string) string {
	// Expand initial ~ to OS specific home directory.
	if strings.HasPrefix(path, "~") {
		homeDir := filepath.Dir(lndHomeDir)
		path = strings.Replace(path, "~", homeDir, 1)
	}

	// NOTE: The os.ExpandEnv doesn't work with Windows-style %VARIABLE%,
	// but the variables can still be expanded via POSIX-style $VARIABLE.
	return filepath.Clean(os.ExpandEnv(path))
}
