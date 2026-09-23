package app

import (
	"fmt"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type concurrencyLoad struct {
	InUse      int     `json:"current_in_use"`
	Capacity   int     `json:"max_capacity"`
	Waiting    int     `json:"waiting_in_queue"`
	Percentage float64 `json:"load_percentage"`
}

func (l *concurrencyLoad) add(other concurrencyLoad) {
	l.InUse += other.InUse
	l.Capacity += other.Capacity
	l.Waiting += other.Waiting
	if l.Capacity > 0 {
		l.Percentage = float64(l.InUse) * 100 / float64(l.Capacity)
	}
}

func (a *App) concurrencySnapshot() (map[string]int, map[string]int) {
	a.gatewayMu.Lock()
	defer a.gatewayMu.Unlock()
	return maps.Clone(a.gatewayActive), maps.Clone(a.gatewayWaiting)
}

func (a *App) userConcurrency(w http.ResponseWriter, r *http.Request) error {
	active, waiting := a.concurrencySnapshot()
	at := time.Now().UTC()
	rows, err := a.DB.QueryContext(r.Context(), "SELECT id,email,username,concurrency FROM users WHERE deleted_at IS NULL AND status='active' ORDER BY id")
	if err != nil {
		return err
	}
	defer rows.Close()
	type userLoad struct {
		concurrencyLoad
		ID       int64  `json:"user_id"`
		Email    string `json:"user_email"`
		Username string `json:"username"`
	}
	users := map[int64]*userLoad{}
	for rows.Next() {
		u := &userLoad{}
		var capacity int
		if err := rows.Scan(&u.ID, &u.Email, &u.Username, &capacity); err != nil {
			return err
		}
		key := fmt.Sprintf("user:%d", u.ID)
		if active[key] == 0 && waiting[key] == 0 {
			continue
		}
		u.add(concurrencyLoad{InUse: active[key], Capacity: capacity, Waiting: waiting[key]})
		users[u.ID] = u
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return reply(w, map[string]any{"enabled": true, "timestamp": at, "user": users})
}

func (a *App) accountConcurrency(w http.ResponseWriter, r *http.Request) error {
	platform := strings.TrimSpace(r.URL.Query().Get("platform"))
	if platform != "" && !supportedPlatform(platform) {
		return bad("invalid platform")
	}
	var gid int64
	if raw := strings.TrimSpace(r.URL.Query().Get("group_id")); raw != "" {
		var err error
		gid, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || gid <= 0 {
			return bad("invalid group_id")
		}
	}
	active, waiting := a.concurrencySnapshot()
	at := time.Now().UTC()
	rows, err := a.DB.QueryContext(r.Context(), `SELECT a.id,a.name,a.platform,a.concurrency,COALESCE(g.id,0),COALESCE(g.name,''),COALESCE(g.platform,'')
 FROM accounts a LEFT JOIN account_groups ag ON ag.account_id=a.id LEFT JOIN groups g ON g.id=ag.group_id AND g.deleted_at IS NULL
 WHERE a.deleted_at IS NULL AND a.type='apikey' AND ($1='' OR a.platform=$1) AND ($2::bigint=0 OR g.id=$2) ORDER BY a.id,g.id`, platform, gid)
	if err != nil {
		return err
	}
	defer rows.Close()
	type accountLoad struct {
		concurrencyLoad
		ID        int64  `json:"account_id"`
		Name      string `json:"account_name"`
		Platform  string `json:"platform"`
		GroupID   int64  `json:"group_id"`
		GroupName string `json:"group_name"`
	}
	type groupLoad struct {
		concurrencyLoad
		ID       int64  `json:"group_id"`
		Name     string `json:"group_name"`
		Platform string `json:"platform"`
	}
	type platformLoad struct {
		concurrencyLoad
		Platform string `json:"platform"`
	}
	accounts, groups, platforms := map[int64]*accountLoad{}, map[int64]*groupLoad{}, map[string]*platformLoad{}
	for rows.Next() {
		acc := &accountLoad{}
		var capacity int
		var groupPlatform string
		if err := rows.Scan(&acc.ID, &acc.Name, &acc.Platform, &capacity, &acc.GroupID, &acc.GroupName, &groupPlatform); err != nil {
			return err
		}
		if !supportedPlatform(acc.Platform) {
			continue
		}
		key := fmt.Sprintf("account:%d", acc.ID)
		acc.add(concurrencyLoad{InUse: active[key], Capacity: capacity, Waiting: waiting[key]})
		if accounts[acc.ID] == nil {
			accounts[acc.ID] = acc
			if platforms[acc.Platform] == nil {
				platforms[acc.Platform] = &platformLoad{Platform: acc.Platform}
			}
			platforms[acc.Platform].add(acc.concurrencyLoad)
		}
		// Accounts shared by multiple groups contribute to each group. Group
		// totals are not additive; platform totals count each account once.
		if acc.GroupID != 0 {
			if groups[acc.GroupID] == nil {
				groups[acc.GroupID] = &groupLoad{ID: acc.GroupID, Name: acc.GroupName, Platform: groupPlatform}
			}
			groups[acc.GroupID].add(acc.concurrencyLoad)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return reply(w, map[string]any{"enabled": true, "timestamp": at, "platform": platforms, "group": groups, "account": accounts})
}
