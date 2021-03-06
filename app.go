package main

import (
	crand "crypto/rand"
	"crypto/sha1"
	"database/sql"
	"encoding/binary"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"hash/fnv"
	"bytes"
	"mime/multipart"

	"github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo"
	"github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/middleware"
	"github.com/newrelic/go-agent"
	"github.com/go-redis/redis"
)

const (
	avatarMaxBytes = 1 * 1024 * 1024
	iconsDir = "/home/isucon/icons"
)

var (
	db            *sqlx.DB
	ErrBadReqeust = echo.NewHTTPError(http.StatusBadRequest)
	app newrelic.Application
	rd *redis.Client
	me string
	hosts []string
)

type Renderer struct {
	templates *template.Template
}

func hash(s string) int {
	h := fnv.New32a()
	h.Write([]byte(s))
	return int(h.Sum32())
}

func StartMySQLSegment(txn newrelic.Transaction, collection, operation string) newrelic.DatastoreSegment {
	return newrelic.DatastoreSegment{
		StartTime: txn.StartSegmentNow(),
		Product: newrelic.DatastoreMySQL,
		Collection: collection,
		Operation: operation,
	}
}

func (r *Renderer) Render(w io.Writer, name string, data interface{}, c echo.Context) error {
	return r.templates.ExecuteTemplate(w, name, data)
}

func init() {
	me = os.Getenv("ISUBATA_ME")
	fmt.Println("ME:", me)
	hosts = strings.Split(os.Getenv("ISUBATA_HOSTS"), ",")
	fmt.Println("HOSTS:", hosts)
	seedBuf := make([]byte, 8)
	crand.Read(seedBuf)
	rand.Seed(int64(binary.LittleEndian.Uint64(seedBuf)))

	db_host := os.Getenv("ISUBATA_DB_HOST")
	if db_host == "" {
		db_host = "127.0.0.1"
	}
	db_port := os.Getenv("ISUBATA_DB_PORT")
	if db_port == "" {
		db_port = "3306"
	}
	db_user := os.Getenv("ISUBATA_DB_USER")
	if db_user == "" {
		db_user = "root"
	}
	db_password := os.Getenv("ISUBATA_DB_PASSWORD")
	if db_password != "" {
		db_password = ":" + db_password
	}

	dsn := fmt.Sprintf("%s%s@tcp(%s:%s)/isubata?parseTime=true&loc=Local&charset=utf8mb4",
		db_user, db_password, db_host, db_port)

	log.Printf("Connecting to db: %q", dsn)
	db, _ = sqlx.Connect("mysql", dsn)
	for {
		err := db.Ping()
		if err == nil {
			break
		}
		log.Println(err)
		time.Sleep(time.Second * 3)
	}

	db.SetMaxOpenConns(20)
	db.SetConnMaxLifetime(5 * time.Minute)
	log.Printf("Succeeded to connect db.")

	redis_host := os.Getenv("ISUBATA_REDIS_HOST")
	if redis_host == "" {
		redis_host = "127.0.0.1"
	}
	redis_port := os.Getenv("ISUBATA_REDIS_PORT")
	if redis_port == "" {
		redis_port = "6379"
	}

	rd = redis.NewClient(&redis.Options{
		Addr: redis_host + ":" + redis_port,
		Password: "",
		DB: 0,
	})

	for {
		err := rd.Ping().Err()
		if err == nil {
			break
		}
		log.Println(err)
		time.Sleep(time.Second * 3)
	}

	log.Println("Succeeded to connect redis.")
}

type User struct {
	ID          int64     `json:"-" db:"id"`
	Name        string    `json:"name" db:"name"`
	Salt        string    `json:"-" db:"salt"`
	Password    string    `json:"-" db:"password"`
	DisplayName string    `json:"display_name" db:"display_name"`
	AvatarIcon  string    `json:"avatar_icon" db:"avatar_icon"`
	CreatedAt   time.Time `json:"-" db:"created_at"`
}

func keyHaveread(user, ch int64) string {
	return fmt.Sprintf("haveread:%d:%d", user, ch)
}

func keyMessages(ch int64) string {
	return fmt.Sprintf("messages:%d", ch)
}

func unifyMessage(id, userID int64, content string, createdAt time.Time) string {
	return fmt.Sprintf("%d@%d@%s@%s", id, userID, createdAt.Format("2006/01/02 15:04:05"), content)
}

func splitMessage(unified string) (ID, userID int64, content string, createdAt string) {
	split := strings.SplitN(unified, "@", 4)
	id, err := strconv.Atoi(split[0])
	if err != nil {
		panic(err)
	}
	ID = int64(id)
	uid, err := strconv.Atoi(split[1])
	if err != nil {
		panic(err)
	}
	userID = int64(uid)
	createdAt, content = split[2], split[3]
	return
}

func getUser(txn newrelic.Transaction, userID int64) (*User, error) {
	u := User{}
	s := StartMySQLSegment(txn, "user", "SELECT")
	err := db.Get(&u, "SELECT * FROM user WHERE id = ?", userID)
	s.End()
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		log.Println("Failed to get user:", err)
		return nil, err
	}
	return &u, nil
}

func addMessage(txn newrelic.Transaction, channelID, userID int64, content string) error {
	s := StartMySQLSegment(txn, "message", "INSERT")
	res, err := db.Exec(
		"INSERT INTO message (channel_id, user_id, content, created_at) VALUES (?, ?, ?, NOW())",
		channelID, userID, content)
	s.End()
	if err != nil {
		log.Println("Failed to addMessage1:", err)
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		log.Println("Failed to addMessage2:", err)
		return err
	}
	err = rd.LPush(keyMessages(channelID), unifyMessage(id, userID, content, time.Now())).Err()
	if err != nil {
		log.Println("Failed to addMessage4:", err)
		return err
	}
	return nil
}

type Message struct {
	ID        int64     `db:"id"`
	ChannelID int64     `db:"channel_id"`
	UserID    int64     `db:"user_id"`
	Content   string    `db:"content"`
	CreatedAt time.Time `db:"created_at"`
}

func sessUserID(c echo.Context) int64 {
	txn := app.StartTransaction("sessUserID", c.Response().Writer, c.Request())
	defer txn.End()
	sess, _ := session.Get("session", c)
	var userID int64
	if x, ok := sess.Values["user_id"]; ok {
		userID, _ = x.(int64)
	}
	return userID
}

func sessSetUserID(c echo.Context, id int64) {
	txn := app.StartTransaction("sessSetUserID", c.Response().Writer, c.Request())
	defer txn.End()
	sess, _ := session.Get("session", c)
	sess.Options = &sessions.Options{
		HttpOnly: true,
		MaxAge:   360000,
	}
	sess.Values["user_id"] = id
	sess.Save(c.Request(), c.Response())
}

func ensureLogin(c echo.Context) (*User, error) {
	txn := app.StartTransaction("ensureLogin", c.Response().Writer, c.Request())
	defer txn.End()
	var user *User
	var err error

	userID := sessUserID(c)
	if userID == 0 {
		c.Redirect(http.StatusSeeOther, "/login")
		return nil, nil
	}

	user, err = getUser(txn, userID)
	if err != nil {
		log.Println("Failed to getUser():", err)
		return nil, err
	}
	if user == nil {
		sess, _ := session.Get("session", c)
		delete(sess.Values, "user_id")
		sess.Save(c.Request(), c.Response())
		c.Redirect(http.StatusSeeOther, "/login")
		return nil, nil
	}
	return user, nil
}

const LettersAndDigits = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func randomString(n int) string {
	b := make([]byte, n)
	z := len(LettersAndDigits)

	for i := 0; i < n; i++ {
		b[i] = LettersAndDigits[rand.Intn(z)]
	}
	return string(b)
}

func register(txn newrelic.Transaction, name, password string) (int64, error) {
	salt := randomString(20)
	digest := fmt.Sprintf("%x", sha1.Sum([]byte(salt+password)))

	s := StartMySQLSegment(txn, "user", "INSERT")
	res, err := db.Exec(
		"INSERT INTO user (name, salt, password, display_name, avatar_icon, created_at)"+
			" VALUES (?, ?, ?, ?, ?, NOW())",
		name, salt, digest, name, "default.png")
	s.End()
	if err != nil {
		log.Println("Failed to register:", err)
		return 0, err
	}
	return res.LastInsertId()
}

// request handlers

func getInitialize(c echo.Context) error {
	txn := app.StartTransaction("getInitialize", c.Response().Writer, c.Request())
	defer txn.End()
	after := time.After(8 * time.Second)
	db.MustExec("DELETE FROM user WHERE id > 1000")
	db.MustExec("DELETE FROM image WHERE id > 1001")
	db.MustExec("DELETE FROM channel WHERE id > 10")
	db.MustExec("DELETE FROM message WHERE id > 10000")
	rd.FlushDB().Err()
	var msgs []Message
	err := db.Select(&msgs, "SELECT * FROM message")
	if err != nil {
		log.Println("Failed to getInitialize:", err)
		return err
	}
	for _, mes := range msgs {
		err := rd.LPush(keyMessages(mes.ChannelID), unifyMessage(mes.ID, mes.UserID, mes.Content, mes.CreatedAt)).Err()
		if err != nil {
			log.Println("Failed to getInitialize1.5:", err)
		}
	}
	os.RemoveAll(iconsDir)
	os.Mkdir(iconsDir, 0777)
	rows, err := db.Query("SELECT name, data FROM image")
	if err != nil {
		log.Println("Failed to preload image:", err)
		return err
	}
	for rows.Next() {
		var name string
		var data []byte
		err := rows.Scan(&name, &data)
		if err != nil {
			log.Println("Failed to scan data:", err)
			return err
		}
		err = ioutil.WriteFile(iconsDir + "/" + name, data, 0777)
		if err != nil {
			log.Println("Failed to write file:", err)
			return err
		}
	}
	rows.Close()
	<-after
	return c.String(204, "")
}

func getIndex(c echo.Context) error {
	txn := app.StartTransaction("getIndex", c.Response().Writer, c.Request())
	defer txn.End()
	userID := sessUserID(c)
	if userID != 0 {
		return c.Redirect(http.StatusSeeOther, "/channel/1")
	}

	return c.Render(http.StatusOK, "index", map[string]interface{}{
		"ChannelID": nil,
	})
}

type ChannelInfo struct {
	ID          int64     `db:"id"`
	Name        string    `db:"name"`
	Description string    `db:"description"`
	UpdatedAt   time.Time `db:"updated_at"`
	CreatedAt   time.Time `db:"created_at"`
}

func getChannel(c echo.Context) error {
	txn := app.StartTransaction("getChannel", c.Response().Writer, c.Request())
	defer txn.End()
	user, err := ensureLogin(c)
	if user == nil {
		return err
	}
	cID, err := strconv.Atoi(c.Param("channel_id"))
	if err != nil {
		log.Println("Failed to getChannel:", err)
		return err
	}
	channels := []ChannelInfo{}
	s := StartMySQLSegment(txn, "channel", "SELECT")
	err = db.Select(&channels, "SELECT * FROM channel ORDER BY id")
	s.End()
	if err != nil {
		log.Println("Failed to getChannel:", err)
		return err
	}

	var desc string
	for _, ch := range channels {
		if ch.ID == int64(cID) {
			desc = ch.Description
			break
		}
	}
	return c.Render(http.StatusOK, "channel", map[string]interface{}{
		"ChannelID":   cID,
		"Channels":    channels,
		"User":        user,
		"Description": desc,
	})
}

func getRegister(c echo.Context) error {
	txn := app.StartTransaction("getRegister", c.Response().Writer, c.Request())
	defer txn.End()
	return c.Render(http.StatusOK, "register", map[string]interface{}{
		"ChannelID": 0,
		"Channels":  []ChannelInfo{},
		"User":      nil,
	})
}

func postRegister(c echo.Context) error {
	txn := app.StartTransaction("postRegister", c.Response().Writer, c.Request())
	defer txn.End()
	name := c.FormValue("name")
	pw := c.FormValue("password")
	if name == "" || pw == "" {
		return ErrBadReqeust
	}
	userID, err := register(txn, name, pw)
	if err != nil {
		if merr, ok := err.(*mysql.MySQLError); ok {
			if merr.Number == 1062 { // Duplicate entry xxxx for key zzzz
				return c.NoContent(http.StatusConflict)
			}
		}
		log.Println("Failed to postRegister:", err)
		return err
	}
	sessSetUserID(c, userID)
	return c.Redirect(http.StatusSeeOther, "/")
}

func getLogin(c echo.Context) error {
	txn := app.StartTransaction("getLogin", c.Response().Writer, c.Request())
	defer txn.End()
	return c.Render(http.StatusOK, "login", map[string]interface{}{
		"ChannelID": 0,
		"Channels":  []ChannelInfo{},
		"User":      nil,
	})
}

func postLogin(c echo.Context) error {
	txn := app.StartTransaction("postLogin", c.Response().Writer, c.Request())
	defer txn.End()
	name := c.FormValue("name")
	pw := c.FormValue("password")
	if name == "" || pw == "" {
		return ErrBadReqeust
	}

	var user User
	s := StartMySQLSegment(txn, "user", "SELECT")
	err := db.Get(&user, "SELECT * FROM user WHERE name = ?", name)
	s.End()
	if err == sql.ErrNoRows {
		return echo.ErrForbidden
	} else if err != nil {
		log.Println("Failed to postLogin:", err)
		return err
	}

	digest := fmt.Sprintf("%x", sha1.Sum([]byte(user.Salt+pw)))
	if digest != user.Password {
		return echo.ErrForbidden
	}
	sessSetUserID(c, user.ID)
	return c.Redirect(http.StatusSeeOther, "/")
}

func getLogout(c echo.Context) error {
	txn := app.StartTransaction("getLogout", c.Response().Writer, c.Request())
	defer txn.End()
	sess, _ := session.Get("session", c)
	delete(sess.Values, "user_id")
	sess.Save(c.Request(), c.Response())
	return c.Redirect(http.StatusSeeOther, "/")
}

func postMessage(c echo.Context) error {
	txn := app.StartTransaction("postMessage", c.Response().Writer, c.Request())
	defer txn.End()
	user, err := ensureLogin(c)
	if user == nil {
		return err
	}

	message := c.FormValue("message")
	if message == "" {
		return echo.ErrForbidden
	}

	var chanID int64
	if x, err := strconv.Atoi(c.FormValue("channel_id")); err != nil {
		return echo.ErrForbidden
	} else {
		chanID = int64(x)
	}

	if err := addMessage(txn, chanID, user.ID, message); err != nil {
		log.Println("Failed to postMessage:", err)
		return err
	}

	return c.NoContent(204)
}

func jsonifyMessage(txn newrelic.Transaction, m_id, m_uid int64, m_con, m_at string) (map[string]interface{}, error) {
	u := User{}
	s := StartMySQLSegment(txn, "user", "SELECT")
	err := db.Get(&u, "SELECT name, display_name, avatar_icon FROM user WHERE id = ?",
		m_uid)
	s.End()
	if err != nil {
		log.Println("Failed to jsonifyMessage:", err)
		return nil, err
	}

	r := make(map[string]interface{})
	r["id"] = m_id
	r["user"] = u
	r["date"] = m_at
	r["content"] = m_con
	return r, nil
}

func queryResponse(txn newrelic.Transaction, chanID, oldLastID int64) (response []map[string]interface{}, read int64, err error) {
	response = make([]map[string]interface{}, 0, 100)
	s := StartMySQLSegment(txn, "message", "SELECT")
	rows, err := db.Query("SELECT m.id, m.created_at, m.content, u.name, u.display_name, u.avatar_icon " +
		"FROM message AS m INNER JOIN user AS u ON m.user_id = u.id " +
		"WHERE m.id > ? AND m.channel_id = ? ORDER BY m.id DESC LIMIT 100", oldLastID, chanID)
	defer rows.Close()
	if err != nil {
		s.End()
		log.Println("Failed to queryResponse:", err)
		return nil, 0, err
	}
	for rows.Next() {
		var m Message
		var u User
		err := rows.Scan(&m.ID, &m.CreatedAt, &m.Content, &u.Name, &u.DisplayName, &u.AvatarIcon)
		if err != nil {
			s.End()
			log.Println("Failed to queryResponse:", err)
			return nil, 0, err
		}
		r := make(map[string]interface{})
		r["id"] = m.ID
		r["user"] = u
		r["date"] = m.CreatedAt.Format("2006/01/02 15:04:05")
		r["content"] = m.Content
		response = append(response, r)
	}
	s.End()

	read, err = rd.LLen(keyMessages(chanID)).Result()
	if err != nil {
		log.Println("Failed to queryResponse2:", err)
		return
	}

	l := len(response)
	for i := 0; i < l / 2; i++ {
		response[i], response[l-i-1] = response[l-i-1], response[i]
	}

	return
}

func getMessage(c echo.Context) error {
	txn := app.StartTransaction("getMessage", c.Response().Writer, c.Request())
	defer txn.End()
	userID := sessUserID(c)
	if userID == 0 {
		return c.NoContent(http.StatusForbidden)
	}

	chanID, err := strconv.ParseInt(c.QueryParam("channel_id"), 10, 64)
	if err != nil {
		log.Println("Failed to getMessage:", err)
		return err
	}
	lastID, err := strconv.ParseInt(c.QueryParam("last_message_id"), 10, 64)
	if err != nil {
		log.Println("Failed to getMessage:", err)
		return err
	}

	response, read, err := queryResponse(txn, chanID, lastID)

	if len(response) > 0 {
		err := rd.Set(keyHaveread(userID, chanID), read, 0).Err()
		if err != nil {
			log.Println("Failed to getMessage:", err)
			return err
		}
	}

	return c.JSON(http.StatusOK, response)
}

func queryChannels(txn newrelic.Transaction) ([]int64, error) {
	res := []int64{}
	s := StartMySQLSegment(txn, "channel", "SELECT")
	err := db.Select(&res, "SELECT id FROM channel")
	s.End()
	return res, err
}

func queryHaveRead(txn newrelic.Transaction, userID, chID int64) (int64, error) {
	id, err := rd.Get(keyHaveread(userID, chID)).Int64()
	if err == redis.Nil {
		return 0, nil
	}
	return id, err
}

func fetchUnread(c echo.Context) error {
	txn := app.StartTransaction("fetchUnread", c.Response().Writer, c.Request())
	defer txn.End()
	userID := sessUserID(c)
	if userID == 0 {
		return c.NoContent(http.StatusForbidden)
	}

	time.Sleep(time.Second)

	channels, err := queryChannels(txn)
	if err != nil {
		log.Println("Failed to fetchUnread1:", err)
		return err
	}

	resp := []map[string]interface{}{}

	for _, chID := range channels {
		read, err := queryHaveRead(txn, userID, chID)
		if err != nil {
			log.Println("Failed to fetchUnread2:", err)
			return err
		}

		max, err := rd.LLen(keyMessages(chID)).Result()
		if err == redis.Nil {
			max = 0
		} else if err != nil {
			log.Println("Failed to fetchUnread2.5:", err)
		}

		cnt := max - read
		r := map[string]interface{}{
			"channel_id": chID,
			"unread":     cnt}
		resp = append(resp, r)
	}

	return c.JSON(http.StatusOK, resp)
}

func getHistory(c echo.Context) error {
	txn := app.StartTransaction("getHistory", c.Response().Writer, c.Request())
	defer txn.End()
	chID, err := strconv.ParseInt(c.Param("channel_id"), 10, 64)
	if err != nil || chID <= 0 {
		return ErrBadReqeust
	}

	user, err := ensureLogin(c)
	if user == nil {
		return err
	}

	var page int64
	pageStr := c.QueryParam("page")
	if pageStr == "" {
		page = 1
	} else {
		page, err = strconv.ParseInt(pageStr, 10, 64)
		if err != nil || page < 1 {
			return ErrBadReqeust
		}
	}

	const N = 20
	cnt, err := rd.LLen(keyMessages(chID)).Result()
	if err == redis.Nil {
		cnt = 0
	} else if err != nil {
		log.Println("Failed to getHistory1:", err)
	}
	maxPage := int64(cnt+N-1) / N
	if maxPage == 0 {
		maxPage = 1
	}
	if page > maxPage {
		return ErrBadReqeust
	}

	unifieds, err := rd.LRange(keyMessages(chID), (page - 1) * N, page * N - 1).Result()
	if err != nil {
		log.Println("Failed to getHistory2:", err)
		return err
	}

	mjson := make([]map[string]interface{}, 0)
	for i := len(unifieds) - 1; i >= 0; i-- {
		id, uid, content, at := splitMessage(unifieds[i])
		r, err := jsonifyMessage(txn, id, uid, content, at)
		if err != nil {
			log.Println("Failed to getHistory3:", err)
			return err
		}
		mjson = append(mjson, r)
	}

	channels := []ChannelInfo{}
	s3 := StartMySQLSegment(txn, "channel", "SELECT")
	err = db.Select(&channels, "SELECT * FROM channel ORDER BY id")
	s3.End()
	if err != nil {
		log.Println("Failed to getHistory4:", err)
		return err
	}

	return c.Render(http.StatusOK, "history", map[string]interface{}{
		"ChannelID": chID,
		"Channels":  channels,
		"Messages":  mjson,
		"MaxPage":   maxPage,
		"Page":      page,
		"User":      user,
	})
}

func getProfile(c echo.Context) error {
	txn := app.StartTransaction("getProfile", c.Response().Writer, c.Request())
	defer txn.End()
	self, err := ensureLogin(c)
	if self == nil {
		return err
	}

	channels := []ChannelInfo{}
	s1 := StartMySQLSegment(txn, "channel", "SELECT")
	err = db.Select(&channels, "SELECT * FROM channel ORDER BY id")
	s1.End()
	if err != nil {
		log.Println("Failed to getProfile1:", err)
		return err
	}

	userName := c.Param("user_name")
	var other User
	s2 := StartMySQLSegment(txn, "user", "SELECT")
	err = db.Get(&other, "SELECT * FROM user WHERE name = ?", userName)
	s2.End()
	if err == sql.ErrNoRows {
		return echo.ErrNotFound
	}
	if err != nil {
		log.Println("Failed to getProfile2:", err)
		return err
	}

	return c.Render(http.StatusOK, "profile", map[string]interface{}{
		"ChannelID":   0,
		"Channels":    channels,
		"User":        self,
		"Other":       other,
		"SelfProfile": self.ID == other.ID,
	})
}

func getAddChannel(c echo.Context) error {
	txn := app.StartTransaction("getAddChannel", c.Response().Writer, c.Request())
	defer txn.End()
	self, err := ensureLogin(c)
	if self == nil {
		return err
	}

	channels := []ChannelInfo{}
	s := StartMySQLSegment(txn, "channel", "SELECT")
	err = db.Select(&channels, "SELECT * FROM channel ORDER BY id")
	s.End()
	if err != nil {
		log.Println("Failed to getAddChannel:", err)
		return err
	}

	return c.Render(http.StatusOK, "add_channel", map[string]interface{}{
		"ChannelID": 0,
		"Channels":  channels,
		"User":      self,
	})
}

func postAddChannel(c echo.Context) error {
	txn := app.StartTransaction("postAddChannel", c.Response().Writer, c.Request())
	defer txn.End()
	self, err := ensureLogin(c)
	if self == nil {
		return err
	}

	name := c.FormValue("name")
	desc := c.FormValue("description")
	if name == "" || desc == "" {
		return ErrBadReqeust
	}

	s := StartMySQLSegment(txn, "channel", "INSERT")
	res, err := db.Exec(
		"INSERT INTO channel (name, description, updated_at, created_at) VALUES (?, ?, NOW(), NOW())",
		name, desc)
	s.End()
	if err != nil {
		log.Println("Failed to postAddChannel:", err)
		return err
	}
	lastID, _ := res.LastInsertId()
	return c.Redirect(http.StatusSeeOther,
		fmt.Sprintf("/channel/%v", lastID))
}

func postProfile(c echo.Context) error {
	txn := app.StartTransaction("postProfile", c.Response().Writer, c.Request())
	defer txn.End()
	self, err := ensureLogin(c)
	if self == nil {
		return err
	}

	avatarName := ""
	var avatarData []byte

	if fh, err := c.FormFile("avatar_icon"); err == http.ErrMissingFile {
		// no file upload
	} else if err != nil {
		log.Println("Failed to postProfile1:", err)
		return err
	} else {
		dotPos := strings.LastIndexByte(fh.Filename, '.')
		if dotPos < 0 {
			return ErrBadReqeust
		}
		ext := fh.Filename[dotPos:]
		switch ext {
		case ".jpg", ".jpeg", ".png", ".gif":
			break
		default:
			return ErrBadReqeust
		}

		file, err := fh.Open()
		if err != nil {
			log.Println("Failed to PostProfile2:", err)
			return err
		}
		avatarData, _ = ioutil.ReadAll(file)
		file.Close()

		if len(avatarData) > avatarMaxBytes {
			return ErrBadReqeust
		}

		avatarName = fmt.Sprintf("%x%s", sha1.Sum(avatarData), ext)
	}

	if avatarName != "" && len(avatarData) > 0 {
		for _, host := range hosts {
			toURL := "http://" + host + "/icons/" + avatarName
			body := &bytes.Buffer{}
			writer := multipart.NewWriter(body)
			part, err := writer.CreateFormFile("avatar_icon", avatarName)
			if err != nil {
				log.Println("Failed to PostProfile2.1:", err)
				return err
			}
			_, err = io.Copy(part, bytes.NewReader(avatarData))
			if err != nil {
				log.Println("Failed to PostProfile2.2:", err)
				return err
			}
			err = writer.Close()
			if err != nil {
				log.Println("Failed to PostProfile2.3:", err)
				return err
			}
			req, err := http.NewRequest(http.MethodPost, toURL, body)
			req.Header.Set("Content-Type", writer.FormDataContentType())
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				log.Println("Failed to PostProfile2.7:", err)
				return err
			}
			resp.Body.Close()
		}
		s2 := StartMySQLSegment(txn, "user", "UPDATE")
		_, err = db.Exec("UPDATE user SET avatar_icon = ? WHERE id = ?", avatarName, self.ID)
		s2.End()
		if err != nil {
			log.Println("Failed to PostProfile4:", err)
			return err
		}
	}

	if name := c.FormValue("display_name"); name != "" {
		s := StartMySQLSegment(txn, "user", "UPDATE")
		_, err := db.Exec("UPDATE user SET display_name = ? WHERE id = ?", name, self.ID)
		s.End()
		if err != nil {
			log.Println("Failed to PostProfile5:", err)
			return err
		}
	}

	return c.Redirect(http.StatusSeeOther, "/")
}

func getIcon(c echo.Context) error {
	txn := app.StartTransaction("getIcon", c.Response().Writer, c.Request())
	defer txn.End()
	fname := c.Param("file_name")
	fpath := iconsDir + "/" + fname
	if _, err := os.Stat(fpath); os.IsNotExist(err) {
		log.Println("UNEXPECTED load icon from mysql:", fname)
		var name string
		var data []byte
		s := StartMySQLSegment(txn, "image", "SELECT")
		err := db.QueryRow("SELECT name, data FROM image WHERE name = ?",
			fname).Scan(&name, &data)
		s.End()
		if err == sql.ErrNoRows {
			return echo.ErrNotFound
		}
		if err != nil {
			log.Println("Failed to getIcon1:", err)
			return err
		}

		mime := ""
		switch true {
		case strings.HasSuffix(name, ".jpg"), strings.HasSuffix(name, ".jpeg"):
			mime = "image/jpeg"
		case strings.HasSuffix(name, ".png"):
			mime = "image/png"
		case strings.HasSuffix(name, ".gif"):
			mime = "image/gif"
		default:
			return echo.ErrNotFound
		}
		return c.Blob(http.StatusOK, mime, data)
	} else if err != nil {
		log.Println("Failed to getIcon2:", err)
		return err
	}
	ifms := time.Date(2000,1,1,1,1,1,1, time.UTC)
	if c.Request().Header.Get("If-Modified-Since") == ifms.Format(http.TimeFormat) {
		log.Println("return 304")
		c.String(http.StatusNotModified, "")
		return nil
	}
	w := c.Response().Writer
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("Last-Modified", ifms.Format(http.TimeFormat))
	file, err := os.Open(fpath)
	if err != nil {
		log.Println("Failed to getIcon4:", err)
		return err
	}
	defer file.Close()
	http.ServeContent(w, c.Request(), fpath, ifms, file)
	return nil
}

func postIcon(c echo.Context) error {
	txn := app.StartTransaction("postIcon", c.Response().Writer, c.Request())
	defer txn.End()

	avatarName := ""
	var avatarData []byte

	if fh, err := c.FormFile("avatar_icon"); err == http.ErrMissingFile {
		// no file upload
	} else if err != nil {
		log.Println("Failed to postIcon1:", err)
		return err
	} else {
		dotPos := strings.LastIndexByte(fh.Filename, '.')
		if dotPos < 0 {
			return ErrBadReqeust
		}
		ext := fh.Filename[dotPos:]
		switch ext {
		case ".jpg", ".jpeg", ".png", ".gif":
			break
		default:
			return ErrBadReqeust
		}

		file, err := fh.Open()
		if err != nil {
			log.Println("Failed to PostIcon2:", err)
			return err
		}
		avatarData, _ = ioutil.ReadAll(file)
		file.Close()

		if len(avatarData) > avatarMaxBytes {
			return ErrBadReqeust
		}

		avatarName = fmt.Sprintf("%x%s", sha1.Sum(avatarData), ext)
	}

	if avatarName != "" && len(avatarData) > 0 {
		err := ioutil.WriteFile(iconsDir + "/" + avatarName, avatarData, 0777)
		if err != nil {
			log.Println("Failed to PostIcon3:", err)
			return err
		}
	}

	return c.NoContent(http.StatusOK)
}

func tAdd(a, b int64) int64 {
	return a + b
}

func tRange(a, b int64) []int64 {
	r := make([]int64, b-a+1)
	for i := int64(0); i <= (b - a); i++ {
		r[i] = a + i
	}
	return r
}

func main() {
	cfg := newrelic.NewConfig("Isubata", os.Getenv("NEW_RELIC_KEY"))
	a, err := newrelic.NewApplication(cfg)
	if err != nil {
		log.Fatalln("Failed to connect to New Relic:", err)
	}
	app = a

	e := echo.New()
	funcs := template.FuncMap{
		"add":    tAdd,
		"xrange": tRange,
	}
	e.Renderer = &Renderer{
		templates: template.Must(template.New("").Funcs(funcs).ParseGlob("views/*.html")),
	}
	e.Use(session.Middleware(sessions.NewCookieStore([]byte("secretonymoris"))))
	e.Use(middleware.LoggerWithConfig(middleware.LoggerConfig{
		Format: "request:\"${method} ${uri}\" status:${status} latency:${latency} (${latency_human}) bytes:${bytes_out}\n",
	}))
	e.Use(middleware.Static("../public"))

	e.GET("/initialize", getInitialize)
	e.GET("/", getIndex)
	e.GET("/register", getRegister)
	e.POST("/register", postRegister)
	e.GET("/login", getLogin)
	e.POST("/login", postLogin)
	e.GET("/logout", getLogout)

	e.GET("/channel/:channel_id", getChannel)
	e.GET("/message", getMessage)
	e.POST("/message", postMessage)
	e.GET("/fetch", fetchUnread)
	e.GET("/history/:channel_id", getHistory)

	e.GET("/profile/:user_name", getProfile)
	e.POST("/profile", postProfile)

	e.GET("add_channel", getAddChannel)
	e.POST("add_channel", postAddChannel)
	e.GET("/icons/:file_name", getIcon)
	e.POST("/icons/:file_name", postIcon)

	e.Start(":5000")
}
