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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	log2 "github.com/labstack/gommon/log"
)

const (
	avatarMaxBytes = 1 * 1024 * 1024
)

var (
	db            *sqlx.DB
	ErrBadReqeust = echo.NewHTTPError(http.StatusBadRequest)
)

type Renderer struct {
	templates *template.Template
}

func (r *Renderer) Render(w io.Writer, name string, data interface{}, c echo.Context) error {
	return r.templates.ExecuteTemplate(w, name, data)
}

func init() {
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

func getUser(userID int64) (*User, error) {
	u := User{}
	if err := db.Get(&u, "SELECT * FROM user WHERE id = ?", userID); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		log.Println(err)
		log.Println(err)
		return nil, err
	}
	return &u, nil
}

func addMessage(channelID, userID int64, content string) (int64, error) {
	res, err := db.Exec(
		"INSERT INTO message (channel_id, user_id, content, created_at) VALUES (?, ?, ?, NOW())",
		channelID, userID, content)
	if err != nil {
		return 0, err
	}
	channelCacher.IncrementMessage(string(channelID))
	return res.LastInsertId()
}

type Message struct {
	ID        int64     `db:"id"`
	ChannelID int64     `db:"channel_id"`
	UserID    int64     `db:"user_id"`
	Content   string    `db:"content"`
	CreatedAt time.Time `db:"created_at"`
	User      *User     `db:"user"`
}

func queryMessages(chanID, lastID int64) ([]Message, error) {
	msgs := []Message{}
	err := db.Select(&msgs, "SELECT * FROM message WHERE id > ? AND channel_id = ? ORDER BY id DESC LIMIT 100",
		lastID, chanID)
	return msgs, err
}

func sessUserID(c echo.Context) int64 {
	sess, _ := session.Get("session", c)
	var userID int64
	if x, ok := sess.Values["user_id"]; ok {
		userID, _ = x.(int64)
	}
	return userID
}

func sessSetUserID(c echo.Context, id int64) {
	sess, _ := session.Get("session", c)
	sess.Options = &sessions.Options{
		HttpOnly: true,
		MaxAge:   360000,
	}
	sess.Values["user_id"] = id
	sess.Save(c.Request(), c.Response())
}

func ensureLogin(c echo.Context) (*User, error) {
	var user *User
	var err error

	userID := sessUserID(c)
	if userID == 0 {
		goto redirect
	}

	user, err = getUser(userID)
	if err != nil {
		log.Println(err)
		return nil, err
	}
	if user == nil {
		sess, _ := session.Get("session", c)
		delete(sess.Values, "user_id")
		sess.Save(c.Request(), c.Response())
		goto redirect
	}
	return user, nil

redirect:
	c.Redirect(http.StatusSeeOther, "/login")
	return nil, nil
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

func register(name, password string) (int64, error) {
	salt := randomString(20)
	digest := fmt.Sprintf("%x", sha1.Sum([]byte(salt+password)))

	res, err := db.Exec(
		"INSERT INTO user (name, salt, password, display_name, avatar_icon, created_at)"+
			" VALUES (?, ?, ?, ?, ?, NOW())",
		name, salt, digest, name, "default.png")
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// request handlers

const iconPath = "/home/isucon/isubata/webapp/public/icons"

type ImageRow struct {
	ID   int32  `db:"id"`
	Data []byte `db:"data"`
	Name string `db:"name"`
}

func getInitialize(c echo.Context) error {
	db.MustExec("DELETE FROM user WHERE id > 1000")
	db.MustExec("DELETE FROM image WHERE id > 1001")
	db.MustExec("DELETE FROM channel WHERE id > 10")
	db.MustExec("DELETE FROM message WHERE id > 10000")
	db.MustExec("DELETE FROM haveread")

	if err := os.RemoveAll(iconPath); err != nil {
		log.Println(err)
	}
	if err := os.MkdirAll(iconPath, os.ModePerm); err != nil {
		log.Println(err)
		return err
	}

	images := make([]*ImageRow, 0, 1001)
	if err := db.Select(&images, "SELECT * FROM image"); err != nil {
		log.Println(err)
		return err
	}
	for _, image := range images {
		if err := os.WriteFile(fmt.Sprintf("%s/%s", iconPath, image.Name), image.Data, os.ModePerm); err != nil {
			log.Println(err)
			return err
		}
	}

	channelCacher = initCannelCacher()
	if _, err := db.Exec("UPDATE channel SET `message_cnt`=0"); err != nil {
		log.Println(err)
		return err
	}
	if _, err := db.Exec("UPDATE channel, (SELECT channel_id, COUNT(*) AS `cnt` FROM message GROUP BY channel_id) AS summary SET `channel`.`message_cnt`=`summary`.`cnt` WHERE `channel`.`id` = `summary`.`channel_id`"); err != nil {
		log.Println(err)
		return err
	}
	channels := make([]*ChannelInfo, 0, 100)
	if err := db.Select(&channels, "SELECT * FROM channel"); err != nil {
		log.Println(err)
		return err
	}
	for _, channel := range channels {
		channelCacher.Set(string(channel.ID), channel, -1)
	}

	req, err := http.NewRequestWithContext(c.Request().Context(), http.MethodGet, "http://172.31.5.58:5000/initialize/isu3", nil)
	if err != nil {
		panic(err)
	}
	_, err = http.DefaultClient.Do(req)
	if err != nil {
		return err
	}

	return c.String(204, "")
}

func getInitializeIsu3(c echo.Context) error {
	channelCacher = initCannelCacher()
	if _, err := db.Exec("UPDATE channel SET `message_cnt`=0"); err != nil {
		log.Println(err)
		return err
	}
	if _, err := db.Exec("UPDATE channel, (SELECT channel_id, COUNT(*) AS `cnt` FROM message GROUP BY channel_id) AS summary SET `channel`.`message_cnt`=`summary`.`cnt` WHERE `channel`.`id` = `summary`.`channel_id`"); err != nil {
		log.Println(err)
		return err
	}
	channels := make([]*ChannelInfo, 0, 100)
	if err := db.Select(&channels, "SELECT * FROM channel"); err != nil {
		log.Println(err)
		return err
	}
	for _, channel := range channels {
		channelCacher.Set(string(channel.ID), channel, -1)
	}

	return c.String(204, "")
}

func getIndex(c echo.Context) error {
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
	MessageCnt  int32     `db:"message_cnt"`
	UpdatedAt   time.Time `db:"updated_at"`
	CreatedAt   time.Time `db:"created_at"`
}

func getChannel(c echo.Context) error {
	user, err := ensureLogin(c)
	if user == nil {
		log.Println(err)
		return err
	}
	cID, err := strconv.Atoi(c.Param("channel_id"))
	if err != nil {
		log.Println(err)
		return err
	}
	channels := channelCacher.GetAll()
	sort.Slice(channels, func(i, j int) bool {
		return channels[i].ID < channels[j].ID
	})

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
	return c.Render(http.StatusOK, "register", map[string]interface{}{
		"ChannelID": 0,
		"Channels":  []ChannelInfo{},
		"User":      nil,
	})
}

func postRegister(c echo.Context) error {
	name := c.FormValue("name")
	pw := c.FormValue("password")
	if name == "" || pw == "" {
		return ErrBadReqeust
	}
	userID, err := register(name, pw)
	if err != nil {
		if merr, ok := err.(*mysql.MySQLError); ok {
			if merr.Number == 1062 { // Duplicate entry xxxx for key zzzz
				return c.NoContent(http.StatusConflict)
			}
		}
		log.Println(err)
		return err
	}
	sessSetUserID(c, userID)
	return c.Redirect(http.StatusSeeOther, "/")
}

func getLogin(c echo.Context) error {
	return c.Render(http.StatusOK, "login", map[string]interface{}{
		"ChannelID": 0,
		"Channels":  []ChannelInfo{},
		"User":      nil,
	})
}

func postLogin(c echo.Context) error {
	name := c.FormValue("name")
	pw := c.FormValue("password")
	if name == "" || pw == "" {
		return ErrBadReqeust
	}

	var user User
	err := db.Get(&user, "SELECT * FROM user WHERE name = ?", name)
	if err == sql.ErrNoRows {
		return echo.ErrForbidden
	} else if err != nil {
		log.Println(err)
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
	sess, _ := session.Get("session", c)
	delete(sess.Values, "user_id")
	sess.Save(c.Request(), c.Response())
	return c.Redirect(http.StatusSeeOther, "/")
}

func postMessage(c echo.Context) error {
	user, err := ensureLogin(c)
	if user == nil {
		log.Println(err)
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

	if _, err := addMessage(chanID, user.ID, message); err != nil {
		log.Println(err)
		return err
	}

	return c.NoContent(204)
}

func jsonifyMessage(m Message) (map[string]interface{}, error) {
	u := User{}
	err := db.Get(&u, "SELECT name, display_name, avatar_icon FROM user WHERE id = ?",
		m.UserID)
	if err != nil {
		log.Println(err)
		return nil, err
	}

	r := make(map[string]interface{})
	r["id"] = m.ID
	r["user"] = u
	r["date"] = m.CreatedAt.Format("2006/01/02 15:04:05")
	r["content"] = m.Content
	return r, nil
}

func querymessagesWithUsers(chanID, lastID int64, limit, offset int32) ([]*Message, error) {
	msgs := make([]*Message, 0)
	query := "SELECT m.*, u.name AS `user.name`, u.avatar_icon AS `user.avatar_icon`, u.display_name AS `user.display_name` FROM message m JOIN user u ON m.user_id = u.id WHERE m.channel_id = ?"

	args := []interface{}{chanID}
	if lastID > 0 {
		args = append(args, lastID)
		query += " AND m.id > ?"
	}
	query += " ORDER BY m.id DESC"
	if limit > 0 {
		args = append(args, limit)
		query += " LIMIT ?"
	}
	if offset > 0 {
		args = append(args, offset)
		query += " OFFSET ?"
	}
	if err := db.Select(&msgs, query, args...); err != nil {
		log.Println(err)
		return nil, err
	}
	return msgs, nil
}

func getMessage(c echo.Context) error {
	userID := sessUserID(c)
	if userID == 0 {
		return c.NoContent(http.StatusForbidden)
	}

	chanID, err := strconv.ParseInt(c.QueryParam("channel_id"), 10, 64)
	if err != nil {
		log.Println(err)
		return err
	}
	lastID, err := strconv.ParseInt(c.QueryParam("last_message_id"), 10, 64)
	if err != nil {
		log.Println(err)
		return err
	}

	messages, err := querymessagesWithUsers(chanID, lastID, 100, 0)
	if err != nil {
		log.Println(err)
		return err
	}

	response := make([]map[string]interface{}, 0, len(messages))
	for i := len(messages) - 1; i >= 0; i-- {
		message := messages[i]
		response = append(response,
			map[string]interface{}{
				"id":      message.ID,
				"user":    message.User,
				"date":    message.CreatedAt.Format("2006/01/02 15:04:05"),
				"content": message.Content,
			},
		)
	}

	if len(messages) > 0 {
		_, err := db.Exec("INSERT INTO haveread (user_id, channel_id, message_id, updated_at, created_at)"+
			" VALUES (?, ?, ?, NOW(), NOW())"+
			" ON DUPLICATE KEY UPDATE message_id = ?, updated_at = NOW()",
			userID, chanID, messages[0].ID, messages[0].ID)
		if err != nil {
			log.Println(err)
			return err
		}
	}

	return c.JSON(http.StatusOK, response)
}

func queryChannels() ([]*ChannelInfo, error) {
	return channelCacher.GetAll(), nil
}

type HaveRead struct {
	UserID    int64     `db:"user_id"`
	ChannelID int64     `db:"channel_id"`
	MessageID int64     `db:"message_id"`
	UpdatedAt time.Time `db:"updated_at"`
	CreatedAt time.Time `db:"created_at"`
}

func queryHaveRead(userID, chID int64) (int64, error) {
	h := HaveRead{}

	err := db.Get(&h, "SELECT * FROM haveread WHERE user_id = ? AND channel_id = ?",
		userID, chID)

	if err == sql.ErrNoRows {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	return h.MessageID, nil
}

func queryHaveReads(userID int64) ([]*HaveRead, error) {
	h := make([]*HaveRead, 0)
	err := db.Select(&h, "SELECT * FROM haveread WHERE user_id = ?", userID)
	if err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		log.Println(err)
		return nil, err
	}
	return h, nil
}

func fetchUnread(c echo.Context) error {
	userID := sessUserID(c)
	if userID == 0 {
		return c.NoContent(http.StatusForbidden)
	}

	channels, err := queryChannels()
	if err != nil {
		log.Println(err)
		return err
	}
	channelMap := make(map[int64]*ChannelInfo, len(channels))
	for _, channel := range channels {
		channelMap[channel.ID] = channel
	}

	haveUnreads, err := queryHaveReads(userID)
	if err != nil {
		log.Println(err)
		log.Println(err)
		return err
	}
	haveUnreadMap := make(map[int64]*HaveRead, len(haveUnreads))
	for _, haveUnread := range haveUnreads {
		haveUnreadMap[haveUnread.ChannelID] = haveUnread
	}

	resp := []map[string]interface{}{}

	for _, channel := range channels {
		var lastID int64
		haveUnread, ok := haveUnreadMap[channel.ID]
		if ok {
			lastID = haveUnread.MessageID
		}

		var cnt int64
		if lastID > 0 {
			err = db.Get(&cnt,
				"SELECT COUNT(*) as cnt FROM message WHERE channel_id = ? AND ? < id",
				channel.ID, lastID)
		} else {
			cnt = int64(channelMap[channel.ID].MessageCnt)
		}
		if err != nil {
			log.Println(err)
			return err
		}
		r := map[string]interface{}{
			"channel_id": channel.ID,
			"unread":     cnt}
		resp = append(resp, r)
	}

	return c.JSON(http.StatusOK, resp)
}

func getHistory(c echo.Context) error {
	chID, err := strconv.ParseInt(c.Param("channel_id"), 10, 64)
	if err != nil || chID <= 0 {
		return ErrBadReqeust
	}

	user, err := ensureLogin(c)
	if user == nil {
		log.Println(err)
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
	var cnt int32
	channel, ok := channelCacher.Get(string(chID))
	if ok {
		cnt = channel.MessageCnt
	}
	maxPage := int64(cnt+N-1) / N
	if maxPage == 0 {
		maxPage = 1
	}
	if page > maxPage {
		return ErrBadReqeust
	}

	messages, err := querymessagesWithUsers(chID, 0, N, int32((page-1)*N))
	if err != nil {
		log.Println(err)
		return err
	}

	mjson := make([]map[string]interface{}, 0, len(messages))
	for i := len(messages) - 1; i >= 0; i-- {
		message := messages[i]
		mjson = append(mjson, map[string]interface{}{
			"id":      message.ID,
			"user":    message.User,
			"date":    message.CreatedAt.Format("2006/01/02 15:04:05"),
			"content": message.Content,
		})
	}

	channels := channelCacher.GetAll()
	sort.Slice(channels, func(i, j int) bool {
		return channels[i].ID < channels[j].ID
	})

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
	self, err := ensureLogin(c)
	if self == nil {
		log.Println(err)
		return err
	}

	channels := channelCacher.GetAll()
	sort.Slice(channels, func(i, j int) bool {
		return channels[i].ID < channels[j].ID
	})

	userName := c.Param("user_name")
	var other User
	err = db.Get(&other, "SELECT * FROM user WHERE name = ?", userName)
	if err == sql.ErrNoRows {
		return echo.ErrNotFound
	}
	if err != nil {
		log.Println(err)
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
	self, err := ensureLogin(c)
	if self == nil {
		log.Println(err)
		return err
	}

	channels := channelCacher.GetAll()
	sort.Slice(channels, func(i, j int) bool {
		return channels[i].ID < channels[j].ID
	})

	return c.Render(http.StatusOK, "add_channel", map[string]interface{}{
		"ChannelID": 0,
		"Channels":  channels,
		"User":      self,
	})
}

func postAddChannel(c echo.Context) error {
	self, err := ensureLogin(c)
	if self == nil {
		log.Println(err)
		return err
	}

	name := c.FormValue("name")
	desc := c.FormValue("description")
	if name == "" || desc == "" {
		return ErrBadReqeust
	}

	now := time.Now()
	res, err := db.Exec(
		"INSERT INTO channel (name, description, updated_at, created_at) VALUES (?, ?, ?, ?)",
		name, desc, now, now)
	if err != nil {
		log.Println(err)
		return err
	}

	lastID, _ := res.LastInsertId()

	channelCacher.Set(string(lastID), &ChannelInfo{ID: lastID, Name: name, Description: desc, MessageCnt: 0, UpdatedAt: now, CreatedAt: now}, -1)

	return c.Redirect(http.StatusSeeOther,
		fmt.Sprintf("/channel/%v", lastID))
}

func postProfile(c echo.Context) error {
	self, err := ensureLogin(c)
	if self == nil {
		log.Println(err)
		return err
	}

	avatarName := ""
	var avatarData []byte

	if fh, err := c.FormFile("avatar_icon"); err == http.ErrMissingFile {
		// no file upload
	} else if err != nil {
		log.Println(err)
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
			log.Println(err)
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
		_, err = db.Exec("UPDATE user SET avatar_icon = ? WHERE id = ?", avatarName, self.ID)
		if err != nil {
			log.Println(err)
			return err
		}
		if err := os.WriteFile(fmt.Sprintf("%s/%s", iconPath, avatarName), avatarData, os.ModePerm); err != nil {
			log.Println(err)
			return err
		}
	}

	if name := c.FormValue("display_name"); name != "" {
		_, err := db.Exec("UPDATE user SET display_name = ? WHERE id = ?", name, self.ID)
		if err != nil {
			log.Println(err)
			return err
		}
	}

	return c.Redirect(http.StatusSeeOther, "/")
}

func getIcon(c echo.Context) error {
	var name string
	var data []byte
	err := db.QueryRow("SELECT name, data FROM image WHERE name = ?",
		c.Param("file_name")).Scan(&name, &data)
	if err == sql.ErrNoRows {
		return echo.ErrNotFound
	}
	if err != nil {
		log.Println(err)
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
	e := echo.New()
	log.SetFlags(log.Lshortfile)
	logfile, err := os.OpenFile("/var/log/go.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
	if err != nil {
		panic("cannnot open test.log:" + err.Error())
	}
	log.SetOutput(logfile)
	log.Print("main!!!!")
	e.Logger.SetOutput(logfile)
	e.Logger.SetLevel(log2.ERROR)
	funcs := template.FuncMap{
		"add":    tAdd,
		"xrange": tRange,
	}
	e.Renderer = &Renderer{
		templates: template.Must(template.New("").Funcs(funcs).ParseGlob("views/*.html")),
	}
	e.Use(session.Middleware(sessions.NewCookieStore([]byte("secretonymoris"))))
	e.Use(middleware.Static("../public"))

	e.GET("/initialize", getInitialize)
	e.GET("/initialize/isu3", getInitializeIsu3)
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

	e.Start(":5000")
}

type Cacher[T any] struct {
	Mutex sync.RWMutex
	Cache map[string]struct {
		Value   T
		Expired time.Time
	}
}

func (c *Cacher[T]) Get(key string) (T, bool) {
	c.Mutex.RLock()
	cache, ok := c.Cache[key]
	c.Mutex.RUnlock()
	if ok && (cache.Expired.IsZero() || time.Now().Before(cache.Expired)) {
		return cache.Value, true
	}
	var defaultValue T
	return defaultValue, false
}

func (c *Cacher[T]) GetAll() []T {
	c.Mutex.RLock()
	slice := make([]T, 0, len(c.Cache))
	for _, v := range c.Cache {
		slice = append(slice, v.Value)
	}
	c.Mutex.RUnlock()
	return slice
}

func (c *Cacher[T]) Set(key string, value T, ttl time.Duration) {
	c.Mutex.Lock()
	var expired time.Time
	if ttl > 0 {
		expired = time.Now().Add(ttl)
	}
	c.Cache[key] = struct {
		Value   T
		Expired time.Time
	}{
		Value:   value,
		Expired: expired,
	}
	c.Mutex.Unlock()
}

func (c *Cacher[T]) Delete(key string) {
	c.Mutex.Lock()
	delete(c.Cache, key)
	c.Mutex.Unlock()
}

func (c *Cacher[T]) Flush() {
	c.Mutex.Lock()
	c.Cache = make(map[string]struct {
		Value   T
		Expired time.Time
	})
	c.Mutex.Unlock()
}

type ChannelCacher struct {
	*Cacher[*ChannelInfo]
}

func (c *ChannelCacher) IncrementMessage(key string) {
	c.Mutex.Lock()
	cache, ok := c.Cacher.Cache[key]
	if !ok {
		c.Mutex.Unlock()
		return
	}
	cache.Value.MessageCnt++
	c.Mutex.Unlock()
}

func initCannelCacher() ChannelCacher {
	return ChannelCacher{
		Cacher: &Cacher[*ChannelInfo]{
			Cache: make(map[string]struct {
				Value   *ChannelInfo
				Expired time.Time
			}, 0),
		},
	}
}

var channelCacher = initCannelCacher()
