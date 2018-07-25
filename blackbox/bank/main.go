package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/pkg/errors"
)

func main() {
	var (
		port   = flag.Int("port", 5515, "bank app ranning port")
		dbhost = flag.String("dbhost", "127.0.0.1", "database host")
		dbport = flag.Int("dbport", 3306, "database port")
		dbuser = flag.String("dbuser", "root", "database user")
		dbpass = flag.String("dbpass", "", "database pass")
		dbname = flag.String("dbname", "isubank", "database name")
	)

	flag.Parse()

	addr := fmt.Sprintf(":%d", *port)
	dbup := *dbuser
	if *dbpass != "" {
		dbup += ":" + *dbpass
	}

	dsn := fmt.Sprintf("%s@tcp(%s:%d)/%s?parseTime=true&loc=Local&charset=utf8mb4", dbup, *dbhost, *dbport, *dbname)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("mysql connect failed. err: %s", err)
	}
	server := NewServer(db)

	log.Printf("[INFO] start server %s", addr)
	log.Fatal(http.ListenAndServe(addr, server))
}

func NewServer(db *sql.DB) *http.ServeMux {
	server := http.NewServeMux()

	h := &Handler{db}

	server.HandleFunc("/register", h.Register)
	server.HandleFunc("/add_credit", h.AddCredit)
	server.HandleFunc("/check", h.Check)
	server.HandleFunc("/reserve", h.Reserve)
	server.HandleFunc("/commit", h.Commit)
	server.HandleFunc("/cancel", h.Cancel)

	// default 404
	server.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[INFO] request not found %s", r.URL.RawPath)
		Error(w, "Not found", 404)
	})
	return server
}

const (
	ResOK         = `{"status":"ok"}`
	ResError      = `{"status":"ng","error":"%s"}`
	MySQLDatetime = "2006-01-02 15:04:05"
	LocationName  = "Asia/Tokyo"
)

var (
	CreditIsInsufficient     = errors.New("credit is insufficient")
	ReserveIsExpires         = errors.New("reserve is already expired")
	ReserveIsAlreadyCommited = errors.New("reserve is already commited")
)

func Error(w http.ResponseWriter, err string, code int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	fmt.Fprintln(w, fmt.Sprintf(ResError, err))
}

func Success(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	fmt.Fprintln(w, ResOK)
}

type Handler struct {
	db *sql.DB
}

// Register は POST /register を処理
// ユーザーを作成します。本来はきっととても複雑な処理なのでしょうが誰でも簡単に一瞬で作れるのが特徴です
func (s *Handler) Register(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	type ReqParam struct {
		BankID string `json:"bank_id"`
	}
	req := &ReqParam{}
	if err := json.NewDecoder(r.Body).Decode(req); err != nil {
		Error(w, "can't parse body", http.StatusBadRequest)
		return
	}
	if req.BankID == "" {
		Error(w, "bank_id is required", http.StatusBadRequest)
		return
	}
	if _, err := s.db.Exec(`INSERT INTO user (bank_id, created_at) VALUES (?, NOW())`, req.BankID); err != nil {
		if mysqlError, ok := err.(*mysql.MySQLError); ok {
			if mysqlError.Number == 1062 {
				Error(w, "bank_id already exists", http.StatusBadRequest)
				return
			}
		}
		log.Printf("[WARN] insert user failed. err: %s", err)
		Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	Success(w)
}

// AddCredit は POST /add_credit を処理
// とても簡単に残高を増やすことができます。本当の銀行ならこんなAPIは無いと思いますが...
func (s *Handler) AddCredit(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	type ReqPram struct {
		BankID string `json:"bank_id"`
		Price  int64  `json:"price"`
	}
	req := &ReqPram{}
	if err := json.NewDecoder(r.Body).Decode(req); err != nil {
		Error(w, "can't parse body", http.StatusBadRequest)
		return
	}
	if req.Price <= 0 {
		Error(w, "price must be upper than 0", http.StatusBadRequest)
		return
	}
	userID := s.filterBankID(w, req.BankID)
	if userID <= 0 {
		return
	}
	err := s.txScorp(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`SELECT id FROM user WHERE id = ? LIMIT 1 FOR UPDATE`, userID); err != nil {
			return errors.Wrap(err, "select lock failed")
		}
		return s.modyfyCredit(tx, userID, req.Price, "by add credit API")
	})
	if err != nil {
		log.Printf("[WARN] addCredit failed. err: %s", err)
		Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	Success(w)
}

// Check は POST /check を処理
// 確定済み要求金額を保有しているかどうかを確認します
func (s *Handler) Check(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	type ReqPram struct {
		AppID  string `json:"app_id"`
		BankID string `json:"bank_id"`
		Price  int64  `json:"price"`
	}
	req := &ReqPram{}
	if err := json.NewDecoder(r.Body).Decode(req); err != nil {
		Error(w, "can't parse body", http.StatusBadRequest)
		return
	}
	if req.Price <= 0 {
		Error(w, "price must be upper than 0", http.StatusBadRequest)
		return
	}
	userID := s.filterBankID(w, req.BankID)
	if userID <= 0 {
		return
	}
	err := s.txScorp(func(tx *sql.Tx) error {
		var credit int64
		if err := tx.QueryRow(`SELECT credit FROM user WHERE id = ? LIMIT 1 FOR UPDATE`, userID).Scan(&credit); err != nil {
			return errors.Wrap(err, "select credit failed")
		}
		if credit < req.Price {
			return CreditIsInsufficient
		}
		return nil
	})
	// TODO sleepを入れる
	switch {
	case err == CreditIsInsufficient:
		Error(w, "credit is insufficient", http.StatusOK)
	case err != nil:
		log.Printf("[WARN] check failed. err: %s", err)
		Error(w, "internal server error", http.StatusInternalServerError)
	default:
		Success(w)
	}
}

// Reserve は POST /reserve を処理
// 複数の取引をまとめるために1分間以内のCommitを保証します
func (s *Handler) Reserve(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	type ReqPram struct {
		AppID  string `json:"app_id"`
		BankID string `json:"bank_id"`
		Price  int64  `json:"price"`
	}
	req := &ReqPram{}
	if err := json.NewDecoder(r.Body).Decode(req); err != nil {
		Error(w, "can't parse body", http.StatusBadRequest)
		return
	}
	if req.Price == 0 {
		Error(w, "price is 0", http.StatusBadRequest)
		return
	}
	userID := s.filterBankID(w, req.BankID)
	if userID <= 0 {
		return
	}
	// TODO sleepを入れる
	var rsvID int64
	price := req.Price
	memo := fmt.Sprintf("app:%s, price:%d", req.AppID, req.Price)
	err := s.txScorp(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`SELECT id FROM user WHERE id = ? LIMIT 1 FOR UPDATE`, userID); err != nil {
			return errors.Wrap(err, "select lock failed")
		}
		now := time.Now()
		expire := now.Add(time.Minute)
		isMinus := price < 0
		if isMinus {
			var fixed, reserved int64
			if err := tx.QueryRow(`SELECT IFNULL(SUM(amount), 0) FROM credit WHERE user_id = ?`, userID).Scan(&fixed); err != nil {
				return errors.Wrap(err, "calc credit failed")
			}
			if err := tx.QueryRow(`SELECT IFNULL(SUM(amount), 0) FROM reserve WHERE user_id = ? AND is_minus = 1 AND expire_at >= ?`, userID, expire.Format(MySQLDatetime)).Scan(&reserved); err != nil {
				return errors.Wrap(err, "calc reserve failed")
			}
			if fixed+reserved+price < 0 {
				return CreditIsInsufficient
			}
		}
		query := `INSERT INTO reserve (user_id, amount, note, is_minus, created_at, expire_at) VALUES (?, ?, ?, ?, ?, ?)`
		sr, err := tx.Exec(query, userID, price, memo, isMinus, now.Format(MySQLDatetime), expire.Format(MySQLDatetime))
		if err != nil {
			return errors.Wrap(err, "update user.credit failed")
		}
		rsvID, err = sr.LastInsertId()
		return err
	})

	switch {
	case err == CreditIsInsufficient:
		Error(w, "credit is insufficient", http.StatusOK)
	case err != nil:
		log.Printf("[WARN] reserve failed. err: %s", err)
		Error(w, "internal server error", http.StatusInternalServerError)
	default:
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		fmt.Fprintln(w, fmt.Sprintf(`{"status":"ok","reserve_id":%d}`, rsvID))
	}
}

func (s *Handler) Commit(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	type ReqPram struct {
		AppID      string  `json:"app_id"`
		ReserveIDs []int64 `json:"reserve_ids"`
	}
	req := &ReqPram{}
	if err := json.NewDecoder(r.Body).Decode(req); err != nil {
		Error(w, "can't parse body", http.StatusBadRequest)
		return
	}
	if len(req.ReserveIDs) == 0 {
		Error(w, "reserve_ids is required", http.StatusBadRequest)
		return
	}
	// TODO sleepを入れる
	err := s.txScorp(func(tx *sql.Tx) error {
		l := len(req.ReserveIDs)
		holder := "?" + strings.Repeat(",?", l-1)
		rids := make([]interface{}, l)
		for i, v := range req.ReserveIDs {
			rids[i] = v
		}
		// 空振りロックを避けるために個数チェック
		var count int
		query := fmt.Sprintf(`SELECT COUNT(id) FROM reserve WHERE id IN (%s) AND expire_at >= NOW()`, holder)
		if err := tx.QueryRow(query, rids...).Scan(&count); err != nil {
			return errors.Wrap(err, "count reserve failed")
		}
		if count < l {
			return ReserveIsExpires
		}

		// reserveの取得(for update)
		type Reserve struct {
			ID     int64
			UserID int64
			Amount int64
			Note   string
		}
		reserves := make([]Reserve, 0, l)
		query = fmt.Sprintf(`SELECT id, user_id, amount, note FROM reserve WHERE id IN (%s) FOR UPDATE`, holder)
		rows, err := tx.Query(query, rids...)
		if err != nil {
			return errors.Wrap(err, "select reserves failed")
		}
		defer rows.Close()
		for rows.Next() {
			reserve := Reserve{}
			if err := rows.Scan(&reserve.ID, &reserve.UserID, &reserve.Amount, &reserve.Note); err != nil {
				return errors.Wrap(err, "select reserves failed")
			}
			reserves = append(reserves, reserve)
		}
		if err = rows.Err(); err != nil {
			return errors.Wrap(err, "select reserves failed")
		}
		if len(reserves) != l {
			return ReserveIsAlreadyCommited
		}

		// userのlock
		userids := make([]interface{}, l)
		for i, rsv := range reserves {
			userids[i] = rsv.UserID
		}
		query = fmt.Sprintf(`SELECT id FROM user WHERE id IN (%s)  LIMIT 1 FOR UPDATE`, holder)
		if _, err := tx.Exec(query, userids...); err != nil {
			return errors.Wrap(err, "select lock failed")
		}

		// 予約のcreditへの適用
		for _, rsv := range reserves {
			if err := s.modyfyCredit(tx, rsv.UserID, rsv.Amount, rsv.Note); err != nil {
				return errors.Wrapf(err, "modyfyCredit failed %#v", rsv)
			}
		}

		// reserveの削除
		query = fmt.Sprintf(`DELETE FROM reserve WHERE id IN (%s)`, holder)
		if _, err := tx.Exec(query, rids...); err != nil {
			return errors.Wrap(err, "delete reserve failed")
		}
		return nil
	})
	if err != nil {
		if err == ReserveIsExpires || err == ReserveIsAlreadyCommited {
			Error(w, err.Error(), http.StatusBadRequest)
		} else {
			log.Printf("[WARN] commit credit failed. err: %s", err)
			Error(w, "internal server error", http.StatusInternalServerError)
		}
		return
	}
	Success(w)
}

func (s *Handler) Cancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	type ReqPram struct {
		AppID      string  `json:"app_id"`
		ReserveIDs []int64 `json:"reserve_ids"`
	}
	req := &ReqPram{}
	if err := json.NewDecoder(r.Body).Decode(req); err != nil {
		Error(w, "can't parse body", http.StatusBadRequest)
		return
	}
	if len(req.ReserveIDs) == 0 {
		Error(w, "reserve_ids is required", http.StatusBadRequest)
		return
	}
	// TODO sleepを入れる
	err := s.txScorp(func(tx *sql.Tx) error {
		l := len(req.ReserveIDs)
		holder := "?" + strings.Repeat(",?", l-1)
		rids := make([]interface{}, l)
		for i, v := range req.ReserveIDs {
			rids[i] = v
		}
		// 空振りロックを避けるために個数チェック
		var count int
		query := fmt.Sprintf(`SELECT COUNT(id) FROM reserve WHERE id IN (%s)`, holder)
		if err := tx.QueryRow(query, rids...).Scan(&count); err != nil {
			return errors.Wrap(err, "count reserve failed")
		}
		if count < l {
			return ReserveIsAlreadyCommited
		}

		// reserveの取得(for update)
		type Reserve struct {
			ID     int64
			UserID int64
		}
		reserves := make([]Reserve, 0, l)
		query = fmt.Sprintf(`SELECT id, user_id FROM reserve WHERE id IN (%s) FOR UPDATE`, holder)
		rows, err := tx.Query(query, rids...)
		if err != nil {
			return errors.Wrap(err, "select reserves failed")
		}
		defer rows.Close()
		for rows.Next() {
			reserve := Reserve{}
			if err := rows.Scan(&reserve.ID, &reserve.UserID); err != nil {
				return errors.Wrap(err, "select reserves failed")
			}
			reserves = append(reserves, reserve)
		}
		if err = rows.Err(); err != nil {
			return errors.Wrap(err, "select reserves failed")
		}
		if len(reserves) != l {
			return ReserveIsAlreadyCommited
		}

		// userのlock
		userids := make([]interface{}, l)
		for i, rsv := range reserves {
			userids[i] = rsv.UserID
		}
		query = fmt.Sprintf(`SELECT id FROM user WHERE id IN (%s)  LIMIT 1 FOR UPDATE`, holder)
		if _, err := tx.Exec(query, userids...); err != nil {
			return errors.Wrap(err, "select lock failed")
		}

		// reserveの削除
		query = fmt.Sprintf(`DELETE FROM reserve WHERE id IN (%s)`, holder)
		if _, err := tx.Exec(query, rids...); err != nil {
			return errors.Wrap(err, "delete reserve failed")
		}
		return nil
	})
	if err != nil {
		if err == ReserveIsExpires || err == ReserveIsAlreadyCommited {
			Error(w, err.Error(), http.StatusBadRequest)
		} else {
			log.Printf("[WARN] cancel credit failed. err: %s", err)
			Error(w, "internal server error", http.StatusInternalServerError)
		}
		return
	}
	Success(w)
}

func (s *Handler) filterBankID(w http.ResponseWriter, bankID string) (id int64) {
	if bankID == "" {
		Error(w, "bank_id is required", http.StatusBadRequest)
		return
	}
	err := s.db.QueryRow(`SELECT id FROM user WHERE bank_id = ? LIMIT 1`, bankID).Scan(&id)
	switch {
	case err == sql.ErrNoRows:
		Error(w, "user not found", http.StatusNotFound)
	case err != nil:
		log.Printf("[WARN] get user failed. err: %s", err)
		Error(w, "internal server error", http.StatusInternalServerError)
	}
	return
}

func (s *Handler) txScorp(f func(*sql.Tx) error) (err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return errors.Wrap(err, "begin transaction failed")
	}
	defer func() {
		if e := recover(); e != nil {
			tx.Rollback()
			err = errors.Errorf("panic in transaction: %s", e)
		} else if err != nil {
			tx.Rollback()
		} else {
			err = tx.Commit()
		}
	}()
	err = f(tx)
	return
}

func (s *Handler) modyfyCredit(tx *sql.Tx, userID, price int64, memo string) error {
	if _, err := tx.Exec(`INSERT INTO credit (user_id, amount, note, created_at) VALUES (?, ?, ?, NOW())`, userID, price, memo); err != nil {
		return errors.Wrap(err, "insert credit failed")
	}
	var credit int64
	if err := tx.QueryRow(`SELECT IFNULL(SUM(amount),0) FROM credit WHERE user_id = ?`, userID).Scan(&credit); err != nil {
		return errors.Wrap(err, "calc credit failed")
	}
	if _, err := tx.Exec(`UPDATE user SET credit = ? WHERE id = ?`, credit, userID); err != nil {
		return errors.Wrap(err, "update user.credit failed")
	}
	return nil
}

func init() {
	var err error
	loc, err := time.LoadLocation(LocationName)
	if err != nil {
		log.Panicln(err)
	}
	time.Local = loc
}