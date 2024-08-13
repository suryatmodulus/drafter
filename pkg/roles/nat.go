package roles

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/loopholelabs/drafter/internal/network"
	"github.com/loopholelabs/drafter/pkg/utils"
)

var (
	ErrNotEnoughAvailableIPsInHostCIDR      = errors.New("not enough available IPs in host CIDR")
	ErrNotEnoughAvailableIPsInNamespaceCIDR = errors.New("not enough available IPs in namespace CIDR")
	ErrAllNamespacesClaimed                 = errors.New("all namespaces claimed")
	ErrCouldNotFindHostInterface            = errors.New("could not find host interface")
	ErrCouldNotCreateNAT                    = errors.New("could not create NAT")
	ErrCouldNotOpenHostVethIPs              = errors.New("could not open host Veth IPs")
	ErrCouldNotOpenNamespaceVethIPs         = errors.New("could not open namespace Veth IPs")
	ErrCouldNotReleaseHostVethIP            = errors.New("could not release host Veth IP")
	ErrCouldNotReleaseNamespaceVethIP       = errors.New("could not release namespace Veth IP")
	ErrCouldNotOpenNamespace                = errors.New("could not open namespace")
	ErrCouldNotCloseNamespace               = errors.New("could not close namespace")
	ErrCouldNotRemoveNAT                    = errors.New("could not remove NAT")
	ErrNATContextCancelled                  = errors.New("context for NAT cancelled")
)

type CreateNamespacesHooks struct {
	OnBeforeCreateNamespace func(id string)
	OnBeforeRemoveNamespace func(id string)
}

func CreateNAT(
	ctx context.Context,
	closeCtx context.Context,

	hostInterface string,

	hostVethCIDR string,
	namespaceVethCIDR string,
	blockedSubnetCIDR string,

	namespaceInterface string,
	namespaceInterfaceGateway string,
	namespaceInterfaceNetmask uint32,
	namespaceInterfaceIP string,
	namespaceInterfaceMAC string,

	namespacePrefix string,

	allowIncomingTraffic bool,

	hooks CreateNamespacesHooks,
) (namespaces *NamespacesManager, errs error) {

	// We use the background context here instead of the internal context because we want to distinguish
	// between a context cancellation from the outside and getting a response
	readyCtx, cancelReadyCtx := context.WithCancel(context.Background())
	defer cancelReadyCtx()

	internalCtx, handlePanics, handleGoroutinePanics, cancel, wait, _ := utils.GetPanicHandler(
		ctx,
		&errs,
		utils.GetPanicHandlerHooks{},
	)
	defer wait()
	defer cancel()
	defer handlePanics(false)()

	// Check if the host interface exists
	if _, err := net.InterfaceByName(hostInterface); err != nil {
		panic(errors.Join(ErrCouldNotFindHostInterface, err))
	}

	if err := network.CreateNAT(hostInterface); err != nil {
		panic(errors.Join(ErrCouldNotCreateNAT, err))
	}

	hostVethIPs := network.NewIPTable(hostVethCIDR, internalCtx)
	if err := hostVethIPs.Open(internalCtx); err != nil {
		panic(errors.Join(ErrCouldNotOpenHostVethIPs, err))
	}

	namespaceVethIPs := network.NewIPTable(namespaceVethCIDR, internalCtx)
	if err := namespaceVethIPs.Open(internalCtx); err != nil {
		panic(errors.Join(ErrCouldNotOpenNamespaceVethIPs, err))
	}

	if namespaceVethIPs.AvailableIPs() > hostVethIPs.AvailablePairs() {
		panic(ErrNotEnoughAvailableIPsInHostCIDR)
	}

	availableIPs := namespaceVethIPs.AvailableIPs()
	if availableIPs < 1 {
		panic(ErrNotEnoughAvailableIPsInNamespaceCIDR)
	}

	namespaces = newNamespacesManager(
		namespacesManagerConfig{
			ctx:                       internalCtx,
			closeCtx:                  closeCtx,
			hostInterface:             hostInterface,
			namespaceInterface:        namespaceInterface,
			namespaceInterfaceGateway: namespaceInterfaceGateway,
			namespaceInterfaceNetmask: namespaceInterfaceNetmask,
			namespaceInterfaceIP:      namespaceInterfaceIP,
			namespaceInterfaceMAC:     namespaceInterfaceMAC,
			blockedSubnetCIDR:         blockedSubnetCIDR,
			allowIncomingTraffic:      allowIncomingTraffic,
			hostVethIPs:               hostVethIPs,
			namespaceVethIPs:          namespaceVethIPs,
			hooks:                     hooks,
		})

	// We intentionally don't call `wg.Add` and `wg.Done` here - we are ok with leaking this
	// goroutine since we return the Close func. We still need to `defer handleGoroutinePanic()()` however so that
	// if we cancel the context during this call, we still handle it appropriately
	handleGoroutinePanics(false, func() {
		select {
		// Failure case; we cancelled the internal context before we got a connection
		case <-internalCtx.Done():
			if err := namespaces.Close(); err != nil {
				panic(errors.Join(ErrNATContextCancelled, err))
			}

		// Happy case; we've set up all of the namespaces and we want to wait with closing the agent's connections until the context, not the internal context is cancelled
		case <-readyCtx.Done():
			<-ctx.Done()

			if err := namespaces.Close(); err != nil {
				panic(errors.Join(ErrNATContextCancelled, err))
			}

			break
		}
	})

	for i := uint64(0); i < availableIPs; i++ {
		select {
		case <-ctx.Done():
			panic(ctx.Err())
		default:
		}

		id := fmt.Sprintf("%v%v", namespacePrefix, i)
		if err := namespaces.addClaimableNamespace(id); err != nil {
			panic(err)
		}
	}

	cancelReadyCtx()

	return namespaces, nil
}

type NamespacesManager struct {
	config namespacesManagerConfig

	closeInProgressCtx       context.Context
	cancelCloseInProgressCtx func()

	closed     bool
	closedLock sync.Mutex

	hostVeths     []*network.IPPair
	hostVethsLock sync.Mutex

	namespaceVeths     []*network.IP
	namespaceVethsLock sync.Mutex

	claimableNamespaces     map[string]claimableNamespace
	claimableNamespacesLock sync.Mutex
}

type claimableNamespace struct {
	namespace *network.Namespace
	claimed   bool
}

type namespacesManagerConfig struct {
	ctx                       context.Context
	closeCtx                  context.Context
	hostInterface             string
	namespaceInterface        string
	namespaceInterfaceGateway string
	namespaceInterfaceNetmask uint32
	namespaceInterfaceIP      string
	namespaceInterfaceMAC     string
	blockedSubnetCIDR         string
	allowIncomingTraffic      bool
	hostVethIPs               *network.IPTable
	namespaceVethIPs          *network.IPTable
	hooks                     CreateNamespacesHooks
}

func newNamespacesManager(config namespacesManagerConfig) *NamespacesManager {
	nsm := &NamespacesManager{
		config:              config,
		claimableNamespaces: make(map[string]claimableNamespace),
	}
	nsm.closeInProgressCtx, nsm.cancelCloseInProgressCtx = context.WithCancel(config.closeCtx) // We use `closeContext` here since this simply intercepts `ctx`

	return nsm
}

func (nss *NamespacesManager) ClaimNamespace() (string, error) {
	nss.claimableNamespacesLock.Lock()
	defer nss.claimableNamespacesLock.Unlock()

	for _, namespace := range nss.claimableNamespaces {
		if !namespace.claimed {
			namespace.claimed = true
			return namespace.namespace.GetID(), nil
		}
	}

	return "", ErrAllNamespacesClaimed
}

func (nss *NamespacesManager) ReleaseNamespace(namespace string) error {
	nss.claimableNamespacesLock.Lock()
	defer nss.claimableNamespacesLock.Unlock()

	ns, ok := nss.claimableNamespaces[namespace]
	if !ok {
		// Releasing non-claimed namespaces is a no-op
		return nil
	}
	ns.claimed = false

	return nil
}

// Future-proofing; if we decide that NATing should use a background copy loop like `socat`, we can wait for that loop to finish here and return any errors
func (nss *NamespacesManager) Wait() error {
	<-nss.closeInProgressCtx.Done()
	return nil
}

func (nss *NamespacesManager) Close() (errs error) {
	defer nss.cancelCloseInProgressCtx()

	nss.hostVethsLock.Lock()
	defer nss.hostVethsLock.Unlock()

	for _, hostVeth := range nss.hostVeths {
		if err := nss.config.hostVethIPs.ReleasePair(nss.config.closeCtx, hostVeth); err != nil {
			errs = errors.Join(errs, ErrCouldNotReleaseHostVethIP, err)
		}
	}
	nss.hostVeths = []*network.IPPair{}

	nss.namespaceVethsLock.Lock()
	defer nss.namespaceVethsLock.Unlock()

	for _, namespaceVeth := range nss.namespaceVeths {
		if err := nss.config.namespaceVethIPs.ReleaseIP(nss.config.closeCtx, namespaceVeth); err != nil {
			errs = errors.Join(errs, ErrCouldNotReleaseNamespaceVethIP, err)
		}
	}
	nss.namespaceVeths = []*network.IP{}

	nss.claimableNamespacesLock.Lock()
	defer nss.claimableNamespacesLock.Unlock()

	for _, claimableNamespace := range nss.claimableNamespaces {
		if hook := nss.config.hooks.OnBeforeRemoveNamespace; hook != nil {
			hook(claimableNamespace.namespace.GetID())
		}

		if err := claimableNamespace.namespace.Close(); err != nil {
			errs = errors.Join(errs, ErrCouldNotCloseNamespace, err)
		}
	}
	nss.claimableNamespaces = map[string]claimableNamespace{}

	nss.closedLock.Lock()
	defer nss.closedLock.Unlock()

	if !nss.closed {
		nss.closed = true

		if err := network.RemoveNAT(nss.config.hostInterface); err != nil {
			errs = errors.Join(errs, ErrCouldNotRemoveNAT, err)
		}
	}

	// No need to call `.Wait()` here since `.Wait()` is just waiting for us to cancel the in-progress context

	return errs
}

func (nss *NamespacesManager) addClaimableNamespace(id string) error {
	hostVeth, err := nss.openHostVeth()
	if err != nil {
		return errors.Join(ErrCouldNotOpenHostVethIPs, err)
	}
	nss.hostVeths = append(nss.hostVeths, hostVeth)

	namespaceVeth, err := nss.openNamespaceVeth()
	if err != nil {
		return errors.Join(ErrCouldNotOpenNamespaceVethIPs, err)
	}
	nss.namespaceVeths = append(nss.namespaceVeths, namespaceVeth)

	namespace, err := nss.openNamespace(id, hostVeth, namespaceVeth)
	if err != nil {
		return errors.Join(ErrCouldNotOpenNamespace, err)
	}
	nss.claimableNamespaces[id] = claimableNamespace{
		namespace: namespace,
	}

	return nil
}

func (nss *NamespacesManager) openHostVeth() (*network.IPPair, error) {
	nss.hostVethsLock.Lock()
	defer nss.hostVethsLock.Unlock()

	hostVeth, err := nss.config.hostVethIPs.GetPair(nss.config.ctx)
	if err != nil {
		if e := nss.config.hostVethIPs.ReleasePair(nss.config.closeCtx, hostVeth); e != nil {
			return nil, errors.Join(ErrCouldNotReleaseHostVethIP, err, e)
		}

		return nil, err
	}

	return hostVeth, nil
}

func (nss *NamespacesManager) openNamespaceVeth() (*network.IP, error) {
	nss.namespaceVethsLock.Lock()
	defer nss.namespaceVethsLock.Unlock()

	namespaceVeth, err := nss.config.namespaceVethIPs.GetIP(nss.config.ctx)
	if err != nil {
		if e := nss.config.namespaceVethIPs.ReleaseIP(nss.config.closeCtx, namespaceVeth); e != nil {
			return nil, errors.Join(ErrCouldNotReleaseNamespaceVethIP, err, e)
		}

		return nil, err
	}

	return namespaceVeth, nil
}

func (nss *NamespacesManager) openNamespace(id string, hostVeth *network.IPPair, namespaceVeth *network.IP) (*network.Namespace, error) {
	nss.claimableNamespacesLock.Lock()
	defer nss.claimableNamespacesLock.Unlock()

	if hook := nss.config.hooks.OnBeforeCreateNamespace; hook != nil {
		hook(id)
	}

	namespace := network.NewNamespace(
		id,

		nss.config.hostInterface,
		nss.config.namespaceInterface,

		nss.config.namespaceInterfaceGateway,
		nss.config.namespaceInterfaceNetmask,

		hostVeth.GetFirstIP().String(),
		hostVeth.GetSecondIP().String(),

		nss.config.namespaceInterfaceIP,
		namespaceVeth.String(),

		nss.config.blockedSubnetCIDR,

		nss.config.namespaceInterfaceMAC,

		nss.config.allowIncomingTraffic,
	)
	if err := namespace.Open(); err != nil {
		if e := namespace.Close(); e != nil {
			return nil, errors.Join(err, e)
		}

		return nil, err
	}

	return namespace, nil
}
