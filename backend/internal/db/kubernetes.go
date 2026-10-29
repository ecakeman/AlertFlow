package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const KubernetesSource = "kubernetes"

// KubernetesObject is one Pod or Deployment observation.
type KubernetesObject struct {
	ServiceID       string
	ClusterID       string
	Namespace       string
	Kind            string
	Name            string
	UID             string
	Fingerprint     string
	Snapshot        json.RawMessage
	ResourceVersion string
	Active          bool
	Live            bool
	ObservedAt      int64
	Severity        string
	Summary         string
}

// KubernetesNotice is one Warning or selected Normal event.
type KubernetesNotice struct {
	ServiceID  string
	ClusterID  string
	Namespace  string
	Kind       string
	Name       string
	UID        string
	EventUID   string
	Count      int32
	EventType  string
	Severity   string
	Title      string
	Summary    string
	ObservedAt int64
}

// ApplyKubernetesObject stores the snapshot and, when the semantic fingerprint
// changed, one timeline row. The first observation is first_seen only.
func (db *DB) ApplyKubernetesObject(obj KubernetesObject, present func() bool) error {
	if obj.ServiceID == "" || obj.UID == "" || obj.ClusterID == "" {
		return fmt.Errorf("kubernetes object is missing service, cluster, or uid")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockService(tx, obj.ServiceID); err != nil {
		return err
	}
	if present != nil && !present() {
		return ErrServiceGone
	}
	now := time.Now().Unix()
	if obj.ObservedAt == 0 {
		obj.ObservedAt = now
	}
	var previous string
	var active bool
	err = tx.QueryRow(
		`SELECT fingerprint, active FROM runtime_object_states
		 WHERE cluster_id = $1 AND resource_uid = $2`,
		obj.ClusterID, obj.UID,
	).Scan(&previous, &active)
	if errors.Is(err, sql.ErrNoRows) {
		if !obj.Active {
			return tx.Commit()
		}
		if err := insertKubernetesObject(tx, obj, now); err != nil {
			return err
		}
		if err := writeKubernetesTimeline(tx, obj, "kubernetes."+kindKey(obj.Kind)+".first_seen", "info", "first_seen", now); err != nil {
			return err
		}
		if err := upsertKubernetesSource(tx, obj.ServiceID, "ok", "", obj.ObservedAt, true); err != nil {
			return err
		}
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	if previous == obj.Fingerprint && active == obj.Active {
		if _, err := tx.Exec(
			`UPDATE runtime_object_states SET last_seen_at = $3, updated_at = $4
			 WHERE cluster_id = $1 AND resource_uid = $2`,
			obj.ClusterID, obj.UID, obj.ObservedAt, now,
		); err != nil {
			return err
		}
		if err := upsertKubernetesSource(tx, obj.ServiceID, "ok", "", obj.ObservedAt, true); err != nil {
			return err
		}
		return tx.Commit()
	}
	accuracy := "observed"
	eventType := "kubernetes." + kindKey(obj.Kind) + ".changed"
	if !obj.Active {
		eventType = "kubernetes." + kindKey(obj.Kind) + ".inactive"
	} else if !obj.Live {
		accuracy = "reconciled_after_gap"
	}
	if err := insertKubernetesObject(tx, obj, now); err != nil {
		return err
	}
	if err := writeKubernetesTimeline(tx, obj, eventType, obj.Severity, accuracy, now); err != nil {
		return err
	}
	if err := upsertKubernetesSource(tx, obj.ServiceID, "ok", "", obj.ObservedAt, true); err != nil {
		return err
	}
	return tx.Commit()
}

// ApplyKubernetesNotice writes one event timeline row. A repeated count is ignored.
func (db *DB) ApplyKubernetesNotice(note KubernetesNotice, present func() bool) error {
	if note.ServiceID == "" || note.EventUID == "" {
		return fmt.Errorf("kubernetes event is missing service or uid")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockService(tx, note.ServiceID); err != nil {
		return err
	}
	if present != nil && !present() {
		return ErrServiceGone
	}
	now := time.Now().Unix()
	if note.ObservedAt == 0 {
		note.ObservedAt = now
	}
	version := note.Count
	if version < 1 {
		version = 1
	}
	key := fmt.Sprintf("kubernetes:%s:event:%s:%d", note.ClusterID, note.EventUID, version)
	details, err := json.Marshal(map[string]any{"time_accuracy": "observed", "reason": note.Title})
	if err != nil {
		return err
	}
	if err := insertTimelineTx(tx, timelineEvent{
		Key: key, ServiceID: note.ServiceID, Source: KubernetesSource,
		EventType: note.EventType, Severity: note.Severity, Title: note.Title, Summary: note.Summary,
		OccurredAt: note.ObservedAt, ObservedAt: note.ObservedAt,
		ClusterID: note.ClusterID, Namespace: note.Namespace, ResourceKind: note.Kind,
		ResourceName: note.Name, ResourceUID: note.UID, Details: details,
	}, now); err != nil {
		return err
	}
	if err := upsertKubernetesSource(tx, note.ServiceID, "ok", "", note.ObservedAt, true); err != nil {
		return err
	}
	return tx.Commit()
}

// SetKubernetesSource records collector health for one service. present runs
// after the service lock. A false result returns ErrServiceGone and writes nothing.
func (db *DB) SetKubernetesSource(serviceID, status, errText string, present func() bool) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockService(tx, serviceID); err != nil {
		return err
	}
	if present != nil && !present() {
		return ErrServiceGone
	}
	now := time.Now().Unix()
	if err := upsertKubernetesSource(tx, serviceID, status, errText, now, status == "ok"); err != nil {
		return err
	}
	return tx.Commit()
}

// ReleaseKubernetesMapping marks a mapping that is no longer configured.
// Objects stop being active, and the source becomes disabled. Timeline rows stay.
// This is not an object deletion.
func (db *DB) ReleaseKubernetesMapping(serviceID, clusterID, namespace string, present func() bool) error {
	return db.stopKubernetesObservation(serviceID, clusterID, namespace, "mapping removed", present)
}

// DisableKubernetesCollection closes observation while the mapping remains.
// Objects stop being active and the source becomes disabled. Timeline rows stay.
func (db *DB) DisableKubernetesCollection(serviceID, clusterID, namespace string, present func() bool) error {
	return db.stopKubernetesObservation(serviceID, clusterID, namespace, "collection disabled", present)
}

func (db *DB) stopKubernetesObservation(serviceID, clusterID, namespace, reason string, present func() bool) error {
	if serviceID == "" {
		return fmt.Errorf("kubernetes mapping is missing a service")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockService(tx, serviceID); err != nil {
		return err
	}
	if present != nil && !present() {
		return ErrServiceGone
	}
	now := time.Now().Unix()
	if _, err := tx.Exec(
		`UPDATE runtime_object_states SET active = FALSE, updated_at = $4
		 WHERE service_id = $1 AND cluster_id = $2 AND namespace = $3 AND active`,
		serviceID, clusterID, namespace, now,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO runtime_source_state (
			service_id, source, enabled, status, last_attempt_at, last_success_at,
			last_error_safe, cursor, updated_at
		 ) VALUES ($1, $2, FALSE, 'disabled', $3, NULL, $4, '{}', $3)
		 ON CONFLICT (service_id, source) DO UPDATE SET
			enabled = FALSE,
			status = 'disabled',
			last_error_safe = EXCLUDED.last_error_safe,
			updated_at = EXCLUDED.updated_at`,
		serviceID, KubernetesSource, now, reason,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// DeactivateUnseenKubernetes marks stored objects that were absent from the
// latest list. Each one gets a single inactive timeline row.
func (db *DB) DeactivateUnseenKubernetes(serviceID, clusterID, namespace, kind string, seen []string, observedAt int64, present func() bool) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockService(tx, serviceID); err != nil {
		return err
	}
	if present != nil && !present() {
		return ErrServiceGone
	}
	if seen == nil {
		seen = []string{}
	}
	now := time.Now().Unix()
	if observedAt == 0 {
		observedAt = now
	}
	rows, err := tx.Query(
		`SELECT resource_uid, resource_name, fingerprint, resource_version FROM runtime_object_states
		 WHERE service_id = $1 AND cluster_id = $2 AND namespace = $3 AND resource_kind = $4
		   AND active AND NOT (resource_uid = ANY($5::text[]))`,
		serviceID, clusterID, namespace, kind, seen,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	type gone struct{ uid, name, fingerprint, version string }
	var missing []gone
	for rows.Next() {
		var item gone
		if err := rows.Scan(&item.uid, &item.name, &item.fingerprint, &item.version); err != nil {
			return err
		}
		missing = append(missing, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range missing {
		obj := KubernetesObject{
			ServiceID: serviceID, ClusterID: clusterID, Namespace: namespace, Kind: kind,
			Name: item.name, UID: item.uid, Fingerprint: item.fingerprint, ResourceVersion: item.version, Active: false,
			ObservedAt: observedAt, Severity: "warning", Summary: item.name + " is gone",
			Snapshot: json.RawMessage(`{"active":false}`),
		}
		if err := insertKubernetesObject(tx, obj, now); err != nil {
			return err
		}
		if err := writeKubernetesTimeline(tx, obj, "kubernetes."+kindKey(kind)+".inactive", "warning", "reconciled_after_gap", now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func insertKubernetesObject(tx *sql.Tx, obj KubernetesObject, now int64) error {
	snap := obj.Snapshot
	if len(snap) == 0 {
		snap = json.RawMessage(`{}`)
	}
	_, err := tx.Exec(
		`INSERT INTO runtime_object_states (
			cluster_id, resource_uid, service_id, resource_kind, namespace, resource_name,
			fingerprint, snapshot, resource_version, active, last_seen_at, updated_at
		 ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		 ON CONFLICT (cluster_id, resource_uid) DO UPDATE SET
			service_id = EXCLUDED.service_id,
			resource_kind = EXCLUDED.resource_kind,
			namespace = EXCLUDED.namespace,
			resource_name = EXCLUDED.resource_name,
			fingerprint = EXCLUDED.fingerprint,
			snapshot = EXCLUDED.snapshot,
			resource_version = EXCLUDED.resource_version,
			active = EXCLUDED.active,
			last_seen_at = EXCLUDED.last_seen_at,
			updated_at = EXCLUDED.updated_at`,
		obj.ClusterID, obj.UID, obj.ServiceID, obj.Kind, obj.Namespace, obj.Name,
		obj.Fingerprint, string(snap), obj.ResourceVersion, obj.Active, obj.ObservedAt, now,
	)
	return err
}

func writeKubernetesTimeline(tx *sql.Tx, obj KubernetesObject, eventType, severity, accuracy string, now int64) error {
	if severity == "" {
		severity = "info"
	}
	key := fmt.Sprintf("kubernetes:%s:%s:first_seen", obj.ClusterID, obj.UID)
	if accuracy != "first_seen" {
		// resourceVersion identifies one object version. A later return to the
		// same fingerprint is a new transition; a replay of this version is not.
		version := obj.ResourceVersion
		if version == "" {
			version = "0"
		}
		if !obj.Active {
			key = fmt.Sprintf("kubernetes:%s:%s:inactive:%s", obj.ClusterID, obj.UID, version)
		} else {
			key = fmt.Sprintf("kubernetes:%s:%s:%s", obj.ClusterID, obj.UID, version)
		}
	}
	details, err := json.Marshal(map[string]string{"time_accuracy": accuracy})
	if err != nil {
		return err
	}
	title := obj.Kind + " " + obj.Name
	return insertTimelineTx(tx, timelineEvent{
		Key: key, ServiceID: obj.ServiceID, Source: KubernetesSource,
		EventType: eventType, Severity: severity, Title: title, Summary: obj.Summary,
		OccurredAt: obj.ObservedAt, ObservedAt: obj.ObservedAt,
		ClusterID: obj.ClusterID, Namespace: obj.Namespace, ResourceKind: obj.Kind,
		ResourceName: obj.Name, ResourceUID: obj.UID, Details: details,
	}, now)
}

func upsertKubernetesSource(tx *sql.Tx, serviceID, status, errText string, at int64, success bool) error {
	var lastSuccess any
	if success {
		lastSuccess = at
	}
	_, err := tx.Exec(
		`INSERT INTO runtime_source_state (
			service_id, source, enabled, status, last_attempt_at, last_success_at,
			last_error_safe, cursor, updated_at
		 ) VALUES ($1, $2, TRUE, $3, $4, $5, $6, '{}', $4)
		 ON CONFLICT (service_id, source) DO UPDATE SET
			enabled = TRUE,
			status = EXCLUDED.status,
			last_attempt_at = EXCLUDED.last_attempt_at,
			last_success_at = COALESCE(EXCLUDED.last_success_at, runtime_source_state.last_success_at),
			last_error_safe = EXCLUDED.last_error_safe,
			updated_at = EXCLUDED.updated_at`,
		serviceID, KubernetesSource, status, at, lastSuccess, errText,
	)
	return err
}

// ListKubernetesObjects returns the stored objects for one service.
func (db *DB) ListKubernetesObjects(serviceID string) ([]KubernetesObject, error) {
	rows, err := db.Query(
		`SELECT cluster_id, namespace, resource_kind, resource_name, resource_uid, active, last_seen_at
		 FROM runtime_object_states WHERE service_id = $1 ORDER BY cluster_id, namespace, resource_name`,
		serviceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KubernetesObject
	for rows.Next() {
		var obj KubernetesObject
		if err := rows.Scan(&obj.ClusterID, &obj.Namespace, &obj.Kind, &obj.Name, &obj.UID, &obj.Active, &obj.ObservedAt); err != nil {
			return nil, err
		}
		obj.ServiceID = serviceID
		out = append(out, obj)
	}
	return out, rows.Err()
}

func kindKey(kind string) string {
	switch kind {
	case "Deployment":
		return "deployment"
	default:
		return "pod"
	}
}
