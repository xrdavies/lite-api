package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

func (a *App) groupOverrides(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	if err = a.DB.QueryRowContext(r.Context(), "SELECT id FROM groups WHERE id=$1 AND deleted_at IS NULL", id).Scan(&id); err != nil {
		return err
	}
	rows, err := a.DB.QueryContext(r.Context(), `SELECT jsonb_strip_nulls(jsonb_build_object('user_id',u.id,'user_name',u.username,'user_email',u.email,'user_notes',COALESCE(u.notes,''),'user_status',u.status,'rate_multiplier',m.rate_multiplier,'rpm_override',m.rpm_override))
 FROM user_group_rate_multipliers m JOIN users u ON u.id=m.user_id AND u.deleted_at IS NULL
 WHERE m.group_id=$1 AND (m.rate_multiplier IS NOT NULL OR m.rpm_override IS NOT NULL) ORDER BY u.id`, id)
	if err != nil {
		return err
	}
	items, err := jsonRows(rows)
	if err != nil {
		return err
	}
	return reply(w, items)
}

func (a *App) saveGroupOverrides(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r)
	if err != nil {
		return err
	}
	field := "rate_multiplier"
	if strings.HasSuffix(r.URL.Path, "/rpm-overrides") {
		field = "rpm_override"
	}
	ids := []int64{}
	values := map[int64]any{}
	add := func(uid int64, value any) error {
		if uid <= 0 {
			return bad("invalid user_id")
		}
		if _, exists := values[uid]; exists {
			return bad("duplicate user_id")
		}
		ids = append(ids, uid)
		values[uid] = value
		return nil
	}
	if r.Method == "PUT" {
		if field == "rate_multiplier" {
			var in struct {
				Entries *[]struct {
					UserID int64        `json:"user_id"`
					Rate   *json.Number `json:"rate_multiplier"`
				} `json:"entries"`
			}
			if err = decode(w, r, &in); err != nil {
				return err
			}
			if in.Entries == nil || len(*in.Entries) > 1000 {
				return bad("entries array is required and must contain at most 1000 users")
			}
			for _, entry := range *in.Entries {
				if entry.Rate == nil || !positiveDecimal(*entry.Rate, 6, 4) {
					return bad("rate_multiplier must be positive with at most four decimal places")
				}
				if err = add(entry.UserID, entry.Rate.String()); err != nil {
					return err
				}
			}
		} else {
			var in struct {
				Entries *[]struct {
					UserID int64 `json:"user_id"`
					RPM    *int  `json:"rpm_override"`
				} `json:"entries"`
			}
			if err = decode(w, r, &in); err != nil {
				return err
			}
			if in.Entries == nil || len(*in.Entries) > 1000 {
				return bad("entries array is required and must contain at most 1000 users")
			}
			for _, entry := range *in.Entries {
				if entry.RPM != nil && (*entry.RPM < 0 || *entry.RPM > 1000000) {
					return bad("rpm_override must be between 0 and 1000000 or null")
				}
				if err = add(entry.UserID, entry.RPM); err != nil {
					return err
				}
			}
		}
	}
	tx, err := a.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Serialize replacements for this group; foreign-key reads can still proceed.
	if err = tx.QueryRowContext(r.Context(), "SELECT id FROM groups WHERE id=$1 AND deleted_at IS NULL FOR NO KEY UPDATE", id).Scan(&id); err != nil {
		return err
	}
	// User edits also lock the user before touching overrides. Lock in ID order
	// before any override writes so replacements cannot deadlock with those edits.
	rows, err := tx.QueryContext(r.Context(), `SELECT id,deleted_at IS NULL FROM users WHERE id=ANY($1) OR id IN (SELECT user_id FROM user_group_rate_multipliers WHERE group_id=$2) ORDER BY id FOR UPDATE`, pq.Array(ids), id)
	if err != nil {
		return err
	}
	live := map[int64]bool{}
	for rows.Next() {
		var uid int64
		var active bool
		if err = rows.Scan(&uid, &active); err != nil {
			rows.Close()
			return err
		}
		live[uid] = active
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, uid := range ids {
		if !live[uid] {
			return bad("user is unavailable")
		}
	}
	if r.Method == "DELETE" && field == "rate_multiplier" {
		// This established endpoint clears the entire entry, including RPM.
		_, err = tx.ExecContext(r.Context(), "DELETE FROM user_group_rate_multipliers WHERE group_id=$1", id)
	} else {
		_, err = tx.ExecContext(r.Context(), "UPDATE user_group_rate_multipliers SET "+field+"=NULL,updated_at=now() WHERE group_id=$1", id)
		if err != nil {
			return err
		}
		for _, uid := range ids {
			if _, err = tx.ExecContext(r.Context(), "INSERT INTO user_group_rate_multipliers(user_id,group_id,"+field+") VALUES($1,$2,$3) ON CONFLICT(user_id,group_id) DO UPDATE SET "+field+"=EXCLUDED."+field+",updated_at=now()", uid, id, values[uid]); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(r.Context(), "DELETE FROM user_group_rate_multipliers WHERE group_id=$1 AND rate_multiplier IS NULL AND rpm_override IS NULL", id)
	}
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return reply(w, map[string]string{"message": "group overrides updated"})
}

func (a *App) userRPMStatus(w http.ResponseWriter, r *http.Request) error {
	uid, err := pathID(r)
	if err != nil {
		return err
	}
	var limit int
	if err = a.DB.QueryRowContext(r.Context(), "SELECT rpm_limit FROM users WHERE id=$1 AND deleted_at IS NULL", uid).Scan(&limit); err != nil {
		return err
	}
	rows, err := a.DB.QueryContext(r.Context(), `SELECT g.id,g.name,COALESCE(m.rpm_override,g.rpm_limit),CASE WHEN m.rpm_override IS NULL THEN 'group' ELSE 'override' END
 FROM groups g LEFT JOIN user_group_rate_multipliers m ON m.group_id=g.id AND m.user_id=$1
 WHERE g.deleted_at IS NULL AND EXISTS(SELECT 1 FROM api_keys k WHERE k.user_id=$1 AND k.group_id=g.id AND k.deleted_at IS NULL) ORDER BY g.id`, uid)
	if err != nil {
		return err
	}
	type groupStatus struct {
		ID     int64  `json:"group_id"`
		Name   string `json:"group_name"`
		Used   int64  `json:"used"`
		Limit  int    `json:"limit"`
		Source string `json:"source"`
	}
	groups := []groupStatus{}
	minute := time.Now().Unix() / 60
	keys := []string{fmt.Sprintf("gateway:rpm:u:%d:%d", uid, minute)}
	for rows.Next() {
		var g groupStatus
		if err = rows.Scan(&g.ID, &g.Name, &g.Limit, &g.Source); err != nil {
			rows.Close()
			return err
		}
		groups = append(groups, g)
		keys = append(keys, fmt.Sprintf("gateway:rpm:g:%d:%d:%d", uid, g.ID, minute))
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	counts, err := a.Redis.MGet(r.Context(), keys...).Result()
	if err != nil {
		return &apiError{503, "request limit service unavailable"}
	}
	used := make([]int64, len(counts))
	for i, raw := range counts {
		if raw == nil {
			continue
		}
		value, ok := raw.(string)
		if !ok {
			return &apiError{503, "invalid request limit counter"}
		}
		used[i], err = strconv.ParseInt(value, 10, 64)
		if err != nil || used[i] < 0 {
			return &apiError{503, "invalid request limit counter"}
		}
	}
	for i := range groups {
		groups[i].Used = used[i+1]
	}
	return reply(w, map[string]any{"user_rpm_used": used[0], "user_rpm_limit": limit, "per_group": groups})
}
