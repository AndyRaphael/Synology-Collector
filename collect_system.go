package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// CollectorState is the coverage state of one collector, per the coverage contract.
type CollectorState string

const (
	StateOK           CollectorState = "ok"
	StatePartial      CollectorState = "partial"
	StateNotInstalled CollectorState = "not_installed"
	StateUnavailable  CollectorState = "unavailable"
	StateError        CollectorState = "error"
)

// SystemInfo is the SYNO.Core.System result.
type SystemInfo struct {
	State        CollectorState `json:"state"`
	StateReason  string         `json:"state_reason,omitempty"`
	Model        string         `json:"model,omitempty"`
	Hostname     string         `json:"hostname,omitempty"`
	Serial       string         `json:"serial,omitempty"`
	VersionFull  string         `json:"version_full,omitempty"`
	VersionShort string         `json:"version_short,omitempty"`
	UptimeSec    int64          `json:"uptime_sec,omitempty"`
}

// Pool is one storage pool (RAID group).
type Pool struct {
	ID         string  `json:"id"`
	Status     string  `json:"status"`
	DeviceType string  `json:"device_type,omitempty"`
	SizeTotal  int64   `json:"size_total"`
	SizeUsed   int64   `json:"size_used"`
	UsedPct    float64 `json:"used_pct"`
}

// Volume is one storage volume. CapacityKnown is false when total/used were
// missing or inconsistent, so absent capacity is never mistaken for a healthy 0%.
type Volume struct {
	ID            string  `json:"id"`
	Name          string  `json:"name,omitempty"`
	Status        string  `json:"status"`
	FsType        string  `json:"fs_type,omitempty"`
	SizeTotal     int64   `json:"size_total"`
	SizeUsed      int64   `json:"size_used"`
	UsedPct       float64 `json:"used_pct"`
	CapacityKnown bool    `json:"capacity_known"`
}

// Disk is one physical drive.
type Disk struct {
	Name      string `json:"name"`
	Model     string `json:"model,omitempty"`
	Serial    string `json:"serial,omitempty"`
	Vendor    string `json:"vendor,omitempty"`
	Status    string `json:"status"`
	TempC     int64  `json:"temp_c,omitempty"`
	SizeTotal int64  `json:"size_total,omitempty"`
}

// StorageInfo is the SYNO.Storage.CGI.Storage result.
type StorageInfo struct {
	State       CollectorState `json:"state"`
	StateReason string         `json:"state_reason,omitempty"`
	Pools       []Pool         `json:"pools"`
	Volumes     []Volume       `json:"volumes"`
	Disks       []Disk         `json:"disks"`

	// machineName is the NAS hostname, which DSM reports here (storageMachineInfo)
	// rather than in SYNO.Core.System. collect() backfills System.Hostname from it.
	machineName string
}

var versionShortRe = regexp.MustCompile(`\d+\.\d+(?:\.\d+)?`)

func extractVersionShort(full string) string {
	return versionShortRe.FindString(full)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func usedPct(total, used int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

// classifyCollectorErr maps a non-network DSMError to a collector state. Network
// errors are handled by the caller as fatal before reaching here.
func classifyCollectorErr(err error) (CollectorState, string) {
	var de *DSMError
	if errors.As(err, &de) {
		switch de.Kind {
		case ErrAPI:
			switch de.Code {
			case 102, 103:
				return StateUnavailable, de.Msg
			case 105:
				return StateUnavailable, "permission denied — the DSM account needs administrator privileges"
			default:
				return StateError, de.Msg
			}
		default:
			return StateError, de.Msg
		}
	}
	return StateError, err.Error()
}

type rawSystem struct {
	Model         FlexString `json:"model"`
	Serial        FlexString `json:"serial"`
	Hostname      FlexString `json:"hostname"`
	Version       FlexString `json:"version"`
	FirmwareVer   FlexString `json:"firmware_ver"`
	VersionString FlexString `json:"version_string"`
	// DSM reports uptime as up_time: a "H:M:S" string (hours may exceed 24), not
	// a numeric uptime. Uptime is kept as a fallback for other DSM shapes.
	UpTime FlexString `json:"up_time"`
	Uptime FlexInt64  `json:"uptime"`
}

// parseUptime converts DSM's up_time ("230:40:35" = hours:minutes:seconds, hours
// unbounded) into seconds. Returns 0 for an empty or malformed value.
func parseUptime(s string) int64 {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 3 {
		return 0
	}
	h, err1 := strconv.ParseInt(parts[0], 10, 64)
	m, err2 := strconv.ParseInt(parts[1], 10, 64)
	sec, err3 := strconv.ParseInt(parts[2], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || h < 0 || m < 0 || sec < 0 {
		return 0
	}
	return h*3600 + m*60 + sec
}

// collectSystem retrieves model/version/hostname. A network error is fatal
// (returned as error); anything else degrades this collector's state.
func collectSystem(ctx context.Context, c *Client) (*SystemInfo, error) {
	si := &SystemInfo{}
	ver, err := c.pickVersion("SYNO.Core.System")
	if err != nil {
		si.State = StateUnavailable
		si.StateReason = err.Error()
		return si, nil
	}
	data, err := c.apiCall(ctx, "SYNO.Core.System", ver, "info", nil)
	if err != nil {
		if k, _ := kindOf(err); k == ErrNetwork {
			return nil, err
		}
		si.State, si.StateReason = classifyCollectorErr(err)
		return si, nil
	}
	var rs rawSystem
	if err := json.Unmarshal(data, &rs); err != nil {
		si.State = StateError
		si.StateReason = "cannot parse system info: " + err.Error()
		return si, nil
	}
	si.Model = firstNonEmpty(string(rs.Model))
	si.Serial = firstNonEmpty(string(rs.Serial))
	si.Hostname = firstNonEmpty(string(rs.Hostname))
	si.VersionFull = firstNonEmpty(string(rs.FirmwareVer), string(rs.VersionString), string(rs.Version))
	si.VersionShort = extractVersionShort(si.VersionFull)
	si.UptimeSec = parseUptime(string(rs.UpTime))
	if si.UptimeSec == 0 {
		si.UptimeSec = int64(rs.Uptime)
	}
	si.State = StateOK
	return si, nil
}

// rawSize uses pointers so an absent total/used is distinguishable from zero.
type rawSize struct {
	Total *FlexInt64 `json:"total"`
	Used  *FlexInt64 `json:"used"`
}

// sizeVals extracts total/used and reports whether the pair is trustworthy:
// both present, total > 0, used in [0, total]. A missing size object or an
// inconsistent pair yields known == false.
func sizeVals(s rawSize) (total, used int64, known bool) {
	if s.Total == nil || s.Used == nil {
		return 0, 0, false
	}
	total, used = int64(*s.Total), int64(*s.Used)
	if total <= 0 || used < 0 || used > total {
		return total, used, false
	}
	return total, used, true
}

type rawPool struct {
	ID         FlexString `json:"id"`
	Status     FlexString `json:"status"`
	DeviceType FlexString `json:"device_type"`
	Size       rawSize    `json:"size"`
}

type rawVolume struct {
	ID          FlexString `json:"id"`
	DisplayName FlexString `json:"display_name"`
	Status      FlexString `json:"status"`
	FsType      FlexString `json:"fs_type"`
	Size        rawSize    `json:"size"`
}

type rawDisk struct {
	ID        FlexString `json:"id"`
	Name      FlexString `json:"name"`
	Model     FlexString `json:"model"`
	Serial    FlexString `json:"serial"`
	Vendor    FlexString `json:"vendor"`
	Status    FlexString `json:"status"`
	Temp      FlexInt64  `json:"temp"`
	SizeTotal FlexInt64  `json:"size_total"`
}

type rawStorage struct {
	StoragePools []rawPool   `json:"storagePools"`
	Volumes      []rawVolume `json:"volumes"`
	Disks        []rawDisk   `json:"disks"`
}

// machineNameFromStorage finds the NAS hostname in a decoded storage response.
// DSM carries it in storageMachineInfo[].nameStr but nests that object at a
// model-dependent depth (not always at the top of data), so this walks the tree
// and prefers the primary unit (lowest order) over any expansion units.
func machineNameFromStorage(v any) string {
	best, bestOrder := "", int64(1)<<62
	var walk func(any)
	walk = func(node any) {
		switch t := node.(type) {
		case map[string]any:
			if arr, ok := t["storageMachineInfo"].([]any); ok {
				for _, e := range arr {
					m, ok := e.(map[string]any)
					if !ok {
						continue
					}
					name, _ := m["nameStr"].(string)
					if strings.TrimSpace(name) == "" {
						continue
					}
					order := int64(1) << 61 // no order → sort after any real order
					if o, ok := m["order"].(float64); ok {
						order = int64(o)
					}
					if order < bestOrder {
						best, bestOrder = strings.TrimSpace(name), order
					}
				}
			}
			for _, child := range t {
				walk(child)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(v)
	return best
}

// collectStorage retrieves pools, volumes, and disks. Volumes==0 is a hard error
// (no meaningful capacity/health data); disks==0 and pools==0 stay OK here and are
// surfaced as warnings by the evaluation stage.
func collectStorage(ctx context.Context, c *Client) (*StorageInfo, error) {
	st := &StorageInfo{Pools: []Pool{}, Volumes: []Volume{}, Disks: []Disk{}}
	ver, err := c.pickVersion("SYNO.Storage.CGI.Storage")
	if err != nil {
		st.State = StateUnavailable
		st.StateReason = err.Error()
		return st, nil
	}
	data, err := c.apiCall(ctx, "SYNO.Storage.CGI.Storage", ver, "load_info", nil)
	if err != nil {
		if k, _ := kindOf(err); k == ErrNetwork {
			return nil, err
		}
		st.State, st.StateReason = classifyCollectorErr(err)
		return st, nil
	}
	var rs rawStorage
	if err := json.Unmarshal(data, &rs); err != nil {
		st.State = StateError
		st.StateReason = "cannot parse storage info: " + err.Error()
		return st, nil
	}

	for _, p := range rs.StoragePools {
		total, used, _ := sizeVals(p.Size)
		pool := Pool{
			ID:         firstNonEmpty(string(p.ID), "pool"),
			Status:     strings.ToLower(firstNonEmpty(string(p.Status))),
			DeviceType: firstNonEmpty(string(p.DeviceType)),
			SizeTotal:  total,
			SizeUsed:   used,
		}
		pool.UsedPct = usedPct(total, used)
		st.Pools = append(st.Pools, pool)
	}
	for i, v := range rs.Volumes {
		total, used, known := sizeVals(v.Size)
		vol := Volume{
			ID:            firstNonEmpty(string(v.ID), fmt.Sprintf("volume_%d", i+1)),
			Name:          firstNonEmpty(string(v.DisplayName), string(v.ID)),
			Status:        strings.ToLower(firstNonEmpty(string(v.Status))),
			FsType:        firstNonEmpty(string(v.FsType)),
			SizeTotal:     total,
			SizeUsed:      used,
			CapacityKnown: known,
		}
		vol.UsedPct = usedPct(total, used)
		st.Volumes = append(st.Volumes, vol)
	}
	// The hostname is nested at a model-dependent depth, so search the decoded
	// tree rather than relying on a fixed field path.
	var generic any
	if err := json.Unmarshal(data, &generic); err == nil {
		st.machineName = machineNameFromStorage(generic)
	}
	for i, d := range rs.Disks {
		disk := Disk{
			Name:      firstNonEmpty(string(d.Name), string(d.ID), fmt.Sprintf("disk_%d", i+1)),
			Model:     firstNonEmpty(string(d.Model)),
			Serial:    firstNonEmpty(string(d.Serial)),
			Vendor:    firstNonEmpty(string(d.Vendor)),
			Status:    strings.ToLower(firstNonEmpty(string(d.Status))),
			TempC:     int64(d.Temp),
			SizeTotal: int64(d.SizeTotal),
		}
		st.Disks = append(st.Disks, disk)
	}

	if len(st.Volumes) == 0 {
		st.State = StateError
		st.StateReason = "storage responded but reported no volumes"
	} else {
		st.State = StateOK
	}
	return st, nil
}
