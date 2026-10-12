package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// WakeupConfigVersion is the only patch version this build can read. A stored
// patch with any other version fails closed instead of running half-understood.
const WakeupConfigVersion = 1

// wakeupField is one sparse patch field. Absent means inherit, an explicit
// JSON null clears an inherited optional value, anything else replaces it.
type wakeupField[T any] struct {
	Set   bool
	Null  bool
	Value T
}

func (f wakeupField[T]) IsZero() bool { return !f.Set }

func (f *wakeupField[T]) UnmarshalJSON(b []byte) error {
	f.Set = true
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		f.Null = true
		return nil
	}
	return json.Unmarshal(b, &f.Value)
}

func (f wakeupField[T]) MarshalJSON() ([]byte, error) {
	if f.Null {
		return []byte("null"), nil
	}
	return json.Marshal(f.Value)
}

// wakeupObject is a nested configuration object (trigger, target, schedule,
// expiry, filters). It always replaces an inherited one as a whole.
type wakeupObject = wakeupField[json.RawMessage]

func requireObject(f wakeupObject) error {
	if f.Set && !f.Null && !bytes.HasPrefix(bytes.TrimSpace(f.Value), []byte("{")) {
		return errors.New("must be an object or null")
	}
	return nil
}

// WakeupConfigPatch is the sparse, versioned configuration one scope can set
// for a rule. Field semantics and the resolution order are in
// ResolveWakeupConfig. It is a data model only: validating a complete resolved
// configuration belongs to the layer that writes definitions.
type WakeupConfigPatch struct {
	Enabled        wakeupField[bool]   `json:"enabled,omitzero"`
	Name           wakeupField[string] `json:"name,omitzero"`
	Trigger        wakeupObject        `json:"trigger,omitzero"`
	Target         wakeupObject        `json:"target,omitzero"`
	Instruction    wakeupField[string] `json:"instruction,omitzero"`
	Mode           wakeupField[string] `json:"mode,omitzero"`
	MaxFires       wakeupField[int]    `json:"max_fires,omitzero"`
	Expiry         wakeupObject        `json:"expiry,omitzero"`
	Schedule       wakeupObject        `json:"schedule,omitzero"`
	RateLimit      wakeupField[int]    `json:"rate_limit,omitzero"`
	AggregateLimit wakeupField[int]    `json:"aggregate_limit,omitzero"`
	Filters        wakeupObject        `json:"filters,omitzero"`
	ActiveRun      wakeupField[string] `json:"active_run,omitzero"`
}

type wakeupPatchWire struct {
	Version int `json:"v"`
	WakeupConfigPatch
}

// DecodeWakeupConfigPatch reads a stored patch strictly: the version must be
// present and supported, and unknown fields are rejected.
func DecodeWakeupConfigPatch(raw []byte) (WakeupConfigPatch, error) {
	var wire wakeupPatchWire
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return WakeupConfigPatch{}, fmt.Errorf("decode wakeup config: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return WakeupConfigPatch{}, errors.New("decode wakeup config: trailing data")
	}
	if wire.Version != WakeupConfigVersion {
		return WakeupConfigPatch{}, fmt.Errorf("decode wakeup config: unsupported version %d", wire.Version)
	}
	for name, f := range map[string]wakeupObject{"trigger": wire.Trigger, "target": wire.Target, "expiry": wire.Expiry, "schedule": wire.Schedule, "filters": wire.Filters} {
		if err := requireObject(f); err != nil {
			return WakeupConfigPatch{}, fmt.Errorf("decode wakeup config: %s %w", name, err)
		}
	}
	return wire.WakeupConfigPatch, nil
}

// MarshalWakeupConfigPatch is the inverse of DecodeWakeupConfigPatch.
func MarshalWakeupConfigPatch(p WakeupConfigPatch) ([]byte, error) {
	return json.Marshal(wakeupPatchWire{Version: WakeupConfigVersion, WakeupConfigPatch: p})
}

// setWakeupFields lists the JSON names of every field a patch sets or clears.
// It reflects over the patch, so a new field is covered the day it is added.
func setWakeupFields(p WakeupConfigPatch) []string {
	var names []string
	v, t := reflect.ValueOf(p), reflect.TypeOf(p)
	for i := range t.NumField() {
		if zero, ok := v.Field(i).Interface().(interface{ IsZero() bool }); ok && !zero.IsZero() {
			names = append(names, strings.Split(t.Field(i).Tag.Get("json"), ",")[0])
		}
	}
	return names
}
