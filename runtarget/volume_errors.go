package runtarget

import (
	"encoding/json"
	"errors"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/protocol"
)

// volumeErrorData is the allowlist for driver error data that crosses the
// daemon and relay boundaries. Re-encoding drops arbitrary driver fields.
func volumeErrorData(verb, code string, raw json.RawMessage) json.RawMessage {
	if code == api.CodeUnavailable {
		return busyData(raw)
	}
	if verb != verbAcquire || code != api.CodeNotFound {
		return nil
	}
	var v protocol.VolumeMissing
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil || v.Reason != protocol.ReasonVolumeMissing || !volumeName.MatchString(v.Name) {
		return nil
	}
	out, _ := json.Marshal(v)
	return out
}

func missingVolume(err error) string {
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeNotFound {
		return ""
	}
	var v protocol.VolumeMissing
	if json.Unmarshal(volumeErrorData(verbAcquire, e.Code, e.Data), &v) != nil {
		return ""
	}
	return v.Name
}
