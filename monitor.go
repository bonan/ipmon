package ipmon

import (
	"context"
	"fmt"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"io"
	"log"
	"net"
	"sort"
	"time"
)

var (
	Debug = log.New(io.Discard, "[DEBUG] ", 0)
)

type Address struct {
	N       netlink.Addr `json:"-"`
	Address string       `json:"address,omitempty"`
	CIDR    int          `json:"mask,omitempty"`
}

type Route struct {
	Destination string `json:"destination,omitempty"`
	Gateway     string `json:"gateway,omitempty"`
	Link        string `json:"link,omitempty"`
	Src         string `json:"src,omitempty"`
	route       netlink.Route
}

type Interface struct {
	Up   bool `json:"up"`
	link netlink.Link
	Addr []*Address `json:"addr"`
}

type Update struct {
	Type    string   `json:"type,omitempty"`
	Change  []string `json:"change,omitempty"`
	Link    string   `json:"link,omitempty"`
	Address *Address `json:"address,omitempty"`
	Gateway string   `json:"gateway,omitempty"`
	Source  string   `json:"source,omitempty"`

	Routes     []*Route              `json:"routes"`
	Interfaces map[string]*Interface `json:"interfaces"`
}

func (u *Update) MarshalEnv() (env []string) {
	env = append(env, fmt.Sprintf("IPMON_TYPE=%s", u.Type))
	if len(u.Change) > 0 {
		env = append(env, fmt.Sprintf("IPMON_CHANGE=%s", u.Change[0]))
	}
	if u.Address != nil {
		env = append(env, fmt.Sprintf("IPMON_ADDR=%s", u.Address.Address))
		env = append(env, fmt.Sprintf("IPMON_MASK=%d", u.Address.CIDR))
	}
	if u.Gateway != "" {
		env = append(env, fmt.Sprintf("IPMON_GW=%s", u.Gateway))
	}
	if u.Source != "" {
		env = append(env, fmt.Sprintf("IPMON_SRC=%s", u.Source))
	}

	for n, inf := range u.Interfaces {
		for _, a := range inf.Addr {
			ip := net.ParseIP(a.Address)
			if ip.IsLinkLocalUnicast() {
				if ip.To4() != nil {
					env = append(env, fmt.Sprintf("IPMON_LL_IPV4_%s=%s", n, a.Address))
				} else if ip.To16() != nil {
					env = append(env, fmt.Sprintf("IPMON_LL_IPV6_%s=%s", n, a.Address))
				}
			}

			if !ip.IsGlobalUnicast() {
				continue
			}
			if ip.To4() != nil {
				if a.N.ValidLft > 0 {
					env = append(env, fmt.Sprintf("IPMON_IPV4_TTL_%s=%d", n, a.N.ValidLft))
				}
				env = append(env, fmt.Sprintf("IPMON_IPV4_%s=%s", n, a.Address))
				env = append(env, fmt.Sprintf("IPMON_IPV4_MASK_%s=%d", n, a.CIDR))
			} else if ip.To16() != nil {
				if ip.IsPrivate() {
					continue
				}
				if a.N.ValidLft > 0 {
					env = append(env, fmt.Sprintf("IPMON_IPV6_TTL_%s=%d", n, a.N.ValidLft))
				}
				env = append(env, fmt.Sprintf("IPMON_IPV6_%s=%s", n, a.Address))
				env = append(env, fmt.Sprintf("IPMON_IPV6_MASK_%s=%d", n, a.CIDR))
			}
		}

		if inf.Up {
			env = append(env, fmt.Sprintf("IPMON_UP_%s=1", n))
		} else {
			env = append(env, fmt.Sprintf("IPMON_UP_%s=0", n))
		}

	}
	if u.Link != "" {
		env = append(env, fmt.Sprintf("IPMON_LINK=%s", u.Link))
	}

	var defRouteIPv4 *Route = nil
	var defRouteIPv6 *Route = nil

	for _, r := range u.Routes {
		if r.route.Dst == nil {
			switch r.route.Protocol {
			case 4:
				if defRouteIPv4 == nil || r.route.Priority < defRouteIPv4.route.Priority {
					defRouteIPv4 = r
				}
			case 6:
				if defRouteIPv6 == nil || r.route.Priority < defRouteIPv6.route.Priority {
					defRouteIPv6 = r
				}
			}
		} else {

		}
	}

	if defRouteIPv4 != nil {
		src := defRouteIPv4.route.Src
		if src.To4() != nil {
			env = append(env, fmt.Sprintf("IPMON_IPV4=%s", src.To4().String()))
		}
		env = append(env, fmt.Sprintf("IPMON_IPV4_IF=%s", defRouteIPv4.Link))
		if defRouteIPv4.route.Gw != nil {
			env = append(env, fmt.Sprintf("IPMON_IPV4_GW=%s", defRouteIPv4.route.Gw.String()))
		}
	}
	if defRouteIPv6 != nil {
		src := defRouteIPv6.route.Src
		if src.To16() != nil {
			env = append(env, fmt.Sprintf("IPMON_IPV6=%s", src.To16().String()))
		}
		env = append(env, fmt.Sprintf("IPMON_IPV6_IF=%s", defRouteIPv6.Link))
		if defRouteIPv6.route.Gw != nil {
			env = append(env, fmt.Sprintf("IPMON_IPV6_GW=%s", defRouteIPv6.route.Gw.String()))
		}
	}

	sort.Strings(env)
	return env
}

func Monitor(ctx context.Context, interval int, fn func(*Update)) error {
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan struct{})
	addrUpd := make(chan netlink.AddrUpdate, 1)
	routeUpd := make(chan netlink.RouteUpdate, 1)
	linkUpd := make(chan netlink.LinkUpdate, 1)
	neighUpd := make(chan netlink.NeighUpdate, 1)

	defer close(done)

	if err := netlink.NeighSubscribeWithOptions(neighUpd, done, netlink.NeighSubscribeOptions{
		ListExisting: true,
	}); err != nil {
		return err
	}
	if err := netlink.AddrSubscribe(addrUpd, done); err != nil {
		return err
	}
	if err := netlink.RouteSubscribe(routeUpd, done); err != nil {
		return err
	}
	if err := netlink.LinkSubscribe(linkUpd, done); err != nil {
		return err
	}

	lastUpdate := genUpdate(nil)
	lastUpdate.Type = "init"
	fn(lastUpdate)

	var tmrCh <-chan time.Time = make(chan time.Time)
	var tmr *time.Ticker

	if interval > 0 {
		tmr = time.NewTicker(time.Duration(interval) * time.Second)
		tmrCh = tmr.C
		defer tmr.Stop()
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case a, op := <-addrUpd:
			if !op {
				return nil
			}
			lastUpdate = genUpdate(lastUpdate)
			if lastUpdate.addrUpdate(a) {
				fn(lastUpdate)
				if tmr != nil {
					tmr.Reset(time.Duration(interval) * time.Second)
				}
			}
		case l, op := <-linkUpd:
			if !op {
				return nil
			}
			lastUpdate = genUpdate(lastUpdate)
			if lastUpdate.linkUpdate(l) {
				fn(lastUpdate)
				if tmr != nil {
					tmr.Reset(time.Duration(interval) * time.Second)
				}
			}
		case r, op := <-routeUpd:
			if !op {
				return nil
			}
			lastUpdate = genUpdate(lastUpdate)
			if lastUpdate.routeUpdate(r) {
				fn(lastUpdate)
				if tmr != nil {
					tmr.Reset(time.Duration(interval) * time.Second)
				}
			}
		case <-tmrCh:
			lastUpdate := genUpdate(nil)
			lastUpdate.Type = "interval"
			fn(lastUpdate)
		}
	}
}

func genUpdate(last *Update) *Update {
	upd := &Update{
		Interfaces: map[string]*Interface{},
	}

	lnkIdx := map[int]string{}

	links, _ := netlink.LinkList()
	for _, link := range links {
		if link == nil || link.Attrs() == nil {
			continue
		}
		lnkIdx[link.Attrs().Index] = link.Attrs().Name
		inf := &Interface{
			link: link,
			Up:   (link.Attrs().Flags & unix.IFF_UP) == unix.IFF_UP,
		}
		addrs, _ := netlink.AddrList(link, netlink.FAMILY_ALL)

		for _, addr := range addrs {
			if !addr.IP.IsGlobalUnicast() && !addr.IP.IsLinkLocalUnicast() {
				continue
			}
			cidr, _ := addr.Mask.Size()
			inf.Addr = append(inf.Addr, &Address{
				Address: addr.IP.String(),
				CIDR:    cidr,
				N:       addr,
			})
		}
		upd.Interfaces[link.Attrs().Name] = inf
	}

	routes, _ := netlink.RouteList(nil, netlink.FAMILY_ALL)
	for _, route := range routes {

		if route.Scope != netlink.SCOPE_UNIVERSE && route.Scope != netlink.SCOPE_LINK {
			continue
		}
		if route.Dst != nil && route.Dst.IP.IsLinkLocalUnicast() {
			continue
		}
		if route.Table != 254 {
			continue
		}

		dst := "default"
		gw := ""
		src := ""
		if route.Dst != nil {
			dst = route.Dst.String()
		}
		if route.Gw != nil {
			gw = route.Gw.String()
		}
		if route.Src != nil {
			src = route.Src.String()
		}

		upd.Routes = append(upd.Routes, &Route{
			route:       route,
			Destination: dst,
			Gateway:     gw,
			Src:         src,
			Link:        lnkIdx[route.LinkIndex],
		})
	}

	return upd
}

func (u *Update) addrUpdate(a netlink.AddrUpdate) bool {
	u.Type = "address"
	cidr, _ := a.LinkAddress.Mask.Size()
	u.Address = &Address{
		Address: a.LinkAddress.IP.String(),
		CIDR:    cidr,
	}
	lnk, _ := netlink.LinkByIndex(a.LinkIndex)
	if lnk != nil && lnk.Attrs() != nil {
		u.Link = lnk.Attrs().Name
	}
	if a.NewAddr {
		u.Change = []string{"add"}
	} else {
		u.Change = []string{"delete"}
	}
	return true
}

func (u *Update) linkUpdate(a netlink.LinkUpdate) bool {
	u.Type = "link"
	if a.Link != nil && a.Link.Attrs() != nil {
		u.Link = a.Link.Attrs().Name
	}
	u.Change = append(u.Change, testFlag(a.Change, a.Flags, unix.IFF_UP, "up", "down")...)
	u.Change = append(u.Change, testFlag(a.Change, a.Flags, unix.IFF_PROMISC, "promisc", "nopromisc")...)
	u.Change = append(u.Change, testFlag(a.Change, a.Flags, unix.IFF_NOARP, "noarp", "arp")...)
	u.Change = append(u.Change, testFlag(a.Change, a.Flags, unix.IFF_BROADCAST, "broadcast", "nobroadcast")...)
	u.Change = append(u.Change, testFlag(a.Change, a.Flags, unix.IFF_LOOPBACK, "loopback", "noloopback")...)
	u.Change = append(u.Change, testFlag(a.Change, a.Flags, unix.IFF_POINTOPOINT, "pointtopoint", "nopointtopoint")...)
	u.Change = append(u.Change, testFlag(a.Change, a.Flags, unix.IFF_MULTICAST, "multicast", "nomulticast")...)

	if a.Change&unix.IFF_UP == 0 {
		return false
	}
	return true
}
func (u *Update) routeUpdate(a netlink.RouteUpdate) bool {
	u.Type = "route"
	if a.Dst != nil {
		cidr, _ := a.Dst.Mask.Size()
		u.Address = &Address{
			Address: a.Dst.IP.String(),
			CIDR:    cidr,
		}
	} else {
		u.Type = "default_route"
	}
	if a.Gw != nil {
		u.Gateway = a.Gw.String()
	}
	if a.Src != nil {
		u.Source = a.Src.String()
	}
	lnk, _ := netlink.LinkByIndex(a.ILinkIndex)
	if lnk != nil && lnk.Attrs() != nil {
		u.Link = lnk.Attrs().Name
	}
	if a.Type == unix.RTM_NEWROUTE {
		u.Change = []string{"add"}
	} else if a.Type == unix.RTM_DELROUTE {
		u.Change = []string{"delete"}
	}
	if a.Scope != netlink.SCOPE_UNIVERSE && a.Scope != netlink.SCOPE_LINK {
		return false
	}
	if a.Table != 254 {
		return false
	}
	return true
}

func testFlag(a, b, c uint32, add, delete string) []string {
	if a&c == 0 {
		return nil
	}
	if b&c == 0 {
		return []string{delete}
	}
	return []string{add}
}
