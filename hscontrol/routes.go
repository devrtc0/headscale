package hscontrol

import (
	"errors"
	"fmt"
	"net/netip"

	v1 "github.com/juanfont/headscale/gen/go/headscale/v1"
	"github.com/rs/zerolog/log"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gorm.io/gorm"
)

var (
	ErrRouteIsNotAvailable = errors.New("route is not available")
	ExitRouteV4            = netip.MustParsePrefix("0.0.0.0/0")
	ExitRouteV6            = netip.MustParsePrefix("::/0")
)

type Route struct {
	gorm.Model

	MachineID uint64
	Machine   Machine
	Prefix    IPPrefix

	Advertised bool
	Enabled    bool
	IsPrimary  bool
}

type Routes []Route

func (r *Route) String() string {
	return fmt.Sprintf("%s:%s", r.Machine, netip.Prefix(r.Prefix).String())
}

func (r *Route) isExitRoute() bool {
	return netip.Prefix(r.Prefix) == ExitRouteV4 || netip.Prefix(r.Prefix) == ExitRouteV6
}

func (rs Routes) toPrefixes() []netip.Prefix {
	prefixes := make([]netip.Prefix, len(rs))
	for i, r := range rs {
		prefixes[i] = netip.Prefix(r.Prefix)
	}

	return prefixes
}

func (hsdb *HSDatabase) GetRoutes() ([]Route, error) {
	var routes []Route
	err := hsdb.db.Preload("Machine").Find(&routes).Error
	if err != nil {
		return nil, err
	}

	return routes, nil
}

func (hsdb *HSDatabase) GetMachineRoutes(m *Machine) ([]Route, error) {
	var routes []Route
	err := hsdb.db.
		Preload("Machine").
		Where("machine_id = ?", m.ID).
		Find(&routes).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	return routes, nil
}

func (hsdb *HSDatabase) GetRoute(id uint64) (*Route, error) {
	var route Route
	err := hsdb.db.Preload("Machine").First(&route, id).Error
	if err != nil {
		return nil, err
	}

	return &route, nil
}

func (hsdb *HSDatabase) EnableRoute(id uint64) error {
	route, err := hsdb.GetRoute(id)
	if err != nil {
		return err
	}

	// Tailscale requires both IPv4 and IPv6 exit routes to
	// be enabled at the same time, as per
	// https://github.com/juanfont/headscale/issues/804#issuecomment-1399314002
	if route.isExitRoute() {
		return hsdb.enableRoutes(&route.Machine, ExitRouteV4.String(), ExitRouteV6.String())
	}

	return hsdb.enableRoutes(&route.Machine, netip.Prefix(route.Prefix).String())
}

func (hsdb *HSDatabase) DisableRoute(id uint64) error {
	route, err := hsdb.GetRoute(id)
	if err != nil {
		return err
	}

	// Tailscale requires both IPv4 and IPv6 exit routes to
	// be enabled at the same time, as per
	// https://github.com/juanfont/headscale/issues/804#issuecomment-1399314002
	if !route.isExitRoute() {
		route.Enabled = false
		route.IsPrimary = false
		err = hsdb.db.Save(route).Error
		if err != nil {
			return err
		}

		return hsdb.handlePrimarySubnetFailover()
	}

	routes, err := hsdb.GetMachineRoutes(&route.Machine)
	if err != nil {
		return err
	}

	for i := range routes {
		if routes[i].isExitRoute() {
			routes[i].Enabled = false
			routes[i].IsPrimary = false
			err = hsdb.db.Save(&routes[i]).Error
			if err != nil {
				return err
			}
		}
	}

	return hsdb.handlePrimarySubnetFailover()
}

func (hsdb *HSDatabase) DeleteRoute(id uint64) error {
	route, err := hsdb.GetRoute(id)
	if err != nil {
		return err
	}

	// Tailscale requires both IPv4 and IPv6 exit routes to
	// be enabled at the same time, as per
	// https://github.com/juanfont/headscale/issues/804#issuecomment-1399314002
	if !route.isExitRoute() {
		if err := hsdb.db.Unscoped().Delete(&route).Error; err != nil {
			return err
		}

		return hsdb.handlePrimarySubnetFailover()
	}

	routes, err := hsdb.GetMachineRoutes(&route.Machine)
	if err != nil {
		return err
	}

	routesToDelete := []Route{}
	for _, r := range routes {
		if r.isExitRoute() {
			routesToDelete = append(routesToDelete, r)
		}
	}

	if err := hsdb.db.Unscoped().Delete(&routesToDelete).Error; err != nil {
		return err
	}

	return hsdb.handlePrimarySubnetFailover()
}

func (hsdb *HSDatabase) DeleteMachineRoutes(m *Machine) error {
	routes, err := hsdb.GetMachineRoutes(m)
	if err != nil {
		return err
	}

	for i := range routes {
		if err := hsdb.db.Unscoped().Delete(&routes[i]).Error; err != nil {
			return err
		}
	}

	return hsdb.handlePrimarySubnetFailover()
}

// isUniquePrefix returns if there is another machine providing the same route already.
func (hsdb *HSDatabase) isUniquePrefix(route Route) bool {
	var count int64
	hsdb.db.
		Model(&Route{}).
		Where("prefix = ? AND machine_id != ? AND advertised = ? AND enabled = ?",
			route.Prefix,
			route.MachineID,
			true, true).Count(&count)

	return count == 0
}

func (hsdb *HSDatabase) getPrimaryRoute(prefix netip.Prefix) (*Route, error) {
	var route Route
	err := hsdb.db.
		Preload("Machine").
		Where("prefix = ? AND advertised = ? AND enabled = ? AND is_primary = ?", IPPrefix(prefix), true, true, true).
		First(&route).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, gorm.ErrRecordNotFound
	}

	return &route, nil
}

// getMachinePrimaryRoutes returns the routes that are enabled and marked as primary (for subnet failover)
// Exit nodes are not considered for this, as they are never marked as Primary.
func (hsdb *HSDatabase) getMachinePrimaryRoutes(m *Machine) ([]Route, error) {
	var routes []Route
	err := hsdb.db.
		Preload("Machine").
		Where("machine_id = ? AND advertised = ? AND enabled = ? AND is_primary = ?", m.ID, true, true, true).
		Find(&routes).Error
	if err != nil {
		return nil, err
	}

	return routes, nil
}

func (hsdb *HSDatabase) processMachineRoutes(machine *Machine) error {
	currentRoutes := []Route{}
	err := hsdb.db.Where("machine_id = ?", machine.ID).Find(&currentRoutes).Error
	if err != nil {
		return err
	}

	advertisedRoutes := map[netip.Prefix]bool{}
	for _, prefix := range machine.HostInfo.RoutableIPs {
		advertisedRoutes[prefix] = false
	}

	for pos, route := range currentRoutes {
		if _, ok := advertisedRoutes[netip.Prefix(route.Prefix)]; ok {
			if !route.Advertised {
				currentRoutes[pos].Advertised = true
				err := hsdb.db.Save(&currentRoutes[pos]).Error
				if err != nil {
					return err
				}
			}
			advertisedRoutes[netip.Prefix(route.Prefix)] = true
		} else if route.Advertised {
			currentRoutes[pos].Advertised = false
			currentRoutes[pos].Enabled = false
			err := hsdb.db.Save(&currentRoutes[pos]).Error
			if err != nil {
				return err
			}
		}
	}

	for prefix, exists := range advertisedRoutes {
		if !exists {
			route := Route{
				MachineID:  machine.ID,
				Prefix:     IPPrefix(prefix),
				Advertised: true,
				Enabled:    false,
			}
			err := hsdb.db.Create(&route).Error
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (hsdb *HSDatabase) handlePrimarySubnetFailover() error {
	// first, get all the enabled routes
	var routes []Route
	err := hsdb.db.
		Preload("Machine").
		Where("advertised = ? AND enabled = ?", true, true).
		Find(&routes).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		log.Error().Err(err).Msg("error getting routes")
	}

	routesChanged := false
	for pos, route := range routes {
		if route.isExitRoute() {
			continue
		}

		if !route.IsPrimary {
			_, err := hsdb.getPrimaryRoute(netip.Prefix(route.Prefix))
			if hsdb.isUniquePrefix(route) || errors.Is(err, gorm.ErrRecordNotFound) {
				log.Info().
					Str("prefix", netip.Prefix(route.Prefix).String()).
					Str("machine", route.Machine.GivenName).
					Msg("Setting primary route")
				routes[pos].IsPrimary = true
				err := hsdb.db.Save(&routes[pos]).Error
				if err != nil {
					log.Error().Err(err).Msg("error marking route as primary")

					return err
				}

				routesChanged = true

				continue
			}
		}

		if route.IsPrimary {
			if route.Machine.isOnline() {
				continue
			}

			// machine offline, find a new primary
			log.Info().
				Str("machine", route.Machine.Hostname).
				Str("prefix", netip.Prefix(route.Prefix).String()).
				Msgf("machine offline, finding a new primary subnet")

			// find a new primary route
			var newPrimaryRoutes []Route
			err := hsdb.db.
				Preload("Machine").
				Where("prefix = ? AND machine_id != ? AND advertised = ? AND enabled = ?",
					route.Prefix,
					route.MachineID,
					true, true).
				Find(&newPrimaryRoutes).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				log.Error().Err(err).Msg("error finding new primary route")

				return err
			}

			var newPrimaryRoute *Route
			for pos, r := range newPrimaryRoutes {
				if r.Machine.isOnline() {
					newPrimaryRoute = &newPrimaryRoutes[pos]

					break
				}
			}

			if newPrimaryRoute == nil {
				log.Warn().
					Str("machine", route.Machine.Hostname).
					Str("prefix", netip.Prefix(route.Prefix).String()).
					Msgf("no alternative primary route found")

				continue
			}

			log.Info().
				Str("old_machine", route.Machine.Hostname).
				Str("prefix", netip.Prefix(route.Prefix).String()).
				Str("new_machine", newPrimaryRoute.Machine.Hostname).
				Msgf("found new primary route")

			// disable the old primary route
			routes[pos].IsPrimary = false
			err = hsdb.db.Save(&routes[pos]).Error
			if err != nil {
				log.Error().Err(err).Msg("error disabling old primary route")

				return err
			}

			// enable the new primary route
			newPrimaryRoute.IsPrimary = true
			err = hsdb.db.Save(&newPrimaryRoute).Error
			if err != nil {
				log.Error().Err(err).Msg("error enabling new primary route")

				return err
			}

			routesChanged = true
		}
	}

	if routesChanged {
		hsdb.notifyStateChange()
	}

	return nil
}

func (rs Routes) toProto() []*v1.Route {
	protoRoutes := []*v1.Route{}

	for _, route := range rs {
		protoRoute := v1.Route{
			Id:         uint64(route.ID),
			Machine:    route.Machine.toProto(),
			Prefix:     netip.Prefix(route.Prefix).String(),
			Advertised: route.Advertised,
			Enabled:    route.Enabled,
			IsPrimary:  route.IsPrimary,
			CreatedAt:  timestamppb.New(route.CreatedAt),
			UpdatedAt:  timestamppb.New(route.UpdatedAt),
		}

		if route.DeletedAt.Valid {
			protoRoute.DeletedAt = timestamppb.New(route.DeletedAt.Time)
		}

		protoRoutes = append(protoRoutes, &protoRoute)
	}

	return protoRoutes
}