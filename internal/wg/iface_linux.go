//go:build linux

package wg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/ilyapiatykh/seine/internal/logging"
)

// linuxIface is the kernel-WireGuard implementation of Interface. It uses
// netlink for link/address/MTU management and wgctrl for peer/device
// configuration. A single wgctrl client is shared for the lifetime of
// the iface to avoid re-opening the netlink generic socket per call.
type linuxIface struct {
	name string
	wg   *wgctrl.Client
}

func newPlatformInterface(name string) (Interface, error) {
	cli, err := wgctrl.New()
	if err != nil {
		return nil, fmt.Errorf("wg: open wgctrl: %w", err)
	}
	return &linuxIface{name: name, wg: cli}, nil
}

func (l *linuxIface) Name() string { return l.name }

func (l *linuxIface) Up(ctx context.Context, opts UpOptions) error {
	log := logging.FromContext(ctx).With(
		slog.String("component", "wg"),
		slog.String("iface", l.name),
	)

	link, err := l.ensureLink(ctx, opts.MTU)
	if err != nil {
		return err
	}

	// Apply MTU updates after creation in case the link existed already.
	if opts.MTU > 0 && link.Attrs().MTU != opts.MTU {
		if err := netlink.LinkSetMTU(link, opts.MTU); err != nil {
			return fmt.Errorf("wg: set MTU on %s: %w", l.name, err)
		}
	}

	listenPort := opts.ListenPort
	priv := opts.PrivateKey
	cfg := wgtypes.Config{
		PrivateKey:   &priv,
		ListenPort:   &listenPort,
		ReplacePeers: false,
	}
	if err := l.wg.ConfigureDevice(l.name, cfg); err != nil {
		return fmt.Errorf("wg: configure device %s: %w", l.name, err)
	}

	if opts.Address.IsValid() {
		nlAddr, err := netlinkAddrFromPrefix(opts.Address)
		if err != nil {
			return err
		}
		if err := netlink.AddrReplace(link, nlAddr); err != nil {
			return fmt.Errorf("wg: set address %s on %s: %w", opts.Address, l.name, err)
		}
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("wg: link up %s: %w", l.name, err)
	}

	log.Info("interface up",
		slog.Int("listen_port", listenPort),
		slog.String("address", opts.Address.String()),
		slog.Int("mtu", opts.MTU),
	)
	return nil
}

// ensureLink creates the WireGuard link if it does not already exist.
// Returns the netlink.Link in either case.
func (l *linuxIface) ensureLink(ctx context.Context, mtu int) (netlink.Link, error) {
	link, err := netlink.LinkByName(l.name)
	if err == nil {
		return link, nil
	}
	var notFound netlink.LinkNotFoundError
	if !errors.As(err, &notFound) {
		return nil, fmt.Errorf("wg: link lookup %s: %w", l.name, err)
	}

	attrs := netlink.NewLinkAttrs()
	attrs.Name = l.name
	if mtu > 0 {
		attrs.MTU = mtu
	}
	wgLink := &netlink.Wireguard{LinkAttrs: attrs}
	if err := netlink.LinkAdd(wgLink); err != nil {
		return nil, fmt.Errorf("wg: link add %s: %w", l.name, err)
	}
	logging.FromContext(ctx).Info("created wireguard link",
		slog.String("component", "wg"),
		slog.String("iface", l.name),
	)
	return netlink.LinkByName(l.name)
}

func (l *linuxIface) ApplyPeers(ctx context.Context, desired []Peer) (Diff, error) {
	dev, err := l.wg.Device(l.name)
	if err != nil {
		return Diff{}, fmt.Errorf("wg: read device %s: %w", l.name, err)
	}
	current := make(map[wgtypes.Key]Peer, len(dev.Peers))
	for _, p := range dev.Peers {
		current[p.PublicKey] = wgPeerToDomain(p)
	}

	diff := computeDiff(current, desired)
	if diff.Empty() {
		return diff, nil
	}

	cfg := wgtypes.Config{Peers: make([]wgtypes.PeerConfig, 0, len(diff.Added)+len(diff.Updated)+len(diff.Removed))}
	for _, p := range diff.Added {
		cfg.Peers = append(cfg.Peers, peerToConfig(p, false))
	}
	for _, p := range diff.Updated {
		cfg.Peers = append(cfg.Peers, peerToConfig(p, true))
	}
	for _, k := range diff.Removed {
		cfg.Peers = append(cfg.Peers, wgtypes.PeerConfig{PublicKey: k, Remove: true})
	}
	if err := l.wg.ConfigureDevice(l.name, cfg); err != nil {
		return diff, fmt.Errorf("wg: configure peers on %s: %w", l.name, err)
	}

	logging.FromContext(ctx).Info("peers reconciled",
		slog.String("component", "wg"),
		slog.String("iface", l.name),
		slog.Int("added", len(diff.Added)),
		slog.Int("updated", len(diff.Updated)),
		slog.Int("removed", len(diff.Removed)),
	)
	return diff, nil
}

func (l *linuxIface) Status(ctx context.Context) (Status, error) {
	dev, err := l.wg.Device(l.name)
	if err != nil {
		return Status{}, fmt.Errorf("wg: status %s: %w", l.name, err)
	}
	st := Status{
		Name:       dev.Name,
		PublicKey:  dev.PublicKey,
		ListenPort: dev.ListenPort,
		Peers:      make([]PeerStatus, 0, len(dev.Peers)),
	}
	for _, p := range dev.Peers {
		ips := make([]netip.Prefix, 0, len(p.AllowedIPs))
		for _, ipnet := range p.AllowedIPs {
			if pfx, ok := prefixFromIPNet(ipnet); ok {
				ips = append(ips, pfx)
			}
		}
		st.Peers = append(st.Peers, PeerStatus{
			PublicKey:        p.PublicKey,
			Endpoint:         p.Endpoint,
			AllowedIPs:       ips,
			LastHandshake:    p.LastHandshakeTime,
			BytesReceived:    p.ReceiveBytes,
			BytesTransmitted: p.TransmitBytes,
		})
	}
	return st, nil
}

func (l *linuxIface) Down(ctx context.Context) error {
	link, err := netlink.LinkByName(l.name)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if errors.As(err, &notFound) {
			return nil
		}
		return fmt.Errorf("wg: link lookup %s: %w", l.name, err)
	}
	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("wg: link del %s: %w", l.name, err)
	}
	logging.FromContext(ctx).Info("interface removed",
		slog.String("component", "wg"),
		slog.String("iface", l.name),
	)
	return nil
}

func (l *linuxIface) Close() error { return l.wg.Close() }

// --- conversions between domain types and wgctrl/netlink types ---

func peerToConfig(p Peer, replaceAllowedIPs bool) wgtypes.PeerConfig {
	cfg := wgtypes.PeerConfig{
		PublicKey:         p.PublicKey,
		ReplaceAllowedIPs: replaceAllowedIPs,
		Endpoint:          p.Endpoint,
	}
	if p.PersistentKeepalive > 0 {
		ka := p.PersistentKeepalive
		cfg.PersistentKeepaliveInterval = &ka
	}
	for _, pfx := range p.AllowedIPs {
		cfg.AllowedIPs = append(cfg.AllowedIPs, ipNetFromPrefix(pfx))
	}
	return cfg
}

func wgPeerToDomain(p wgtypes.Peer) Peer {
	out := Peer{
		PublicKey:           p.PublicKey,
		Endpoint:            p.Endpoint,
		PersistentKeepalive: p.PersistentKeepaliveInterval,
	}
	for _, ipnet := range p.AllowedIPs {
		if pfx, ok := prefixFromIPNet(ipnet); ok {
			out.AllowedIPs = append(out.AllowedIPs, pfx)
		}
	}
	return out
}

func ipNetFromPrefix(p netip.Prefix) net.IPNet {
	addr := p.Addr()
	bits := p.Bits()
	if addr.Is4() {
		ip4 := addr.As4()
		return net.IPNet{IP: ip4[:], Mask: net.CIDRMask(bits, 32)}
	}
	ip16 := addr.As16()
	return net.IPNet{IP: ip16[:], Mask: net.CIDRMask(bits, 128)}
}

func prefixFromIPNet(n net.IPNet) (netip.Prefix, bool) {
	addr, ok := netip.AddrFromSlice(n.IP)
	if !ok {
		return netip.Prefix{}, false
	}
	addr = addr.Unmap()
	ones, _ := n.Mask.Size()
	return netip.PrefixFrom(addr, ones), true
}

func netlinkAddrFromPrefix(p netip.Prefix) (*netlink.Addr, error) {
	ipnet := ipNetFromPrefix(p)
	addr, err := netlink.ParseAddr(ipnet.String())
	if err != nil {
		return nil, fmt.Errorf("wg: parse addr %s: %w", p, err)
	}
	return addr, nil
}
