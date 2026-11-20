package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

type libraryAccount struct {
	Username string           `json:"username"`
	Salt     []byte           `json:"salt"`
	Hash     []byte           `json:"pbkdf2_sha256_600000"`
	ID       string           `json:"id,omitempty"`
	Members  []libraryAccount `json:"members,omitempty"`
	file     string
	writeMu  sync.Mutex
	mu       sync.RWMutex
}

func copyAccountFields(dst, src *libraryAccount) {
	dst.Username = src.Username
	dst.Salt = append([]byte(nil), src.Salt...)
	dst.Hash = append([]byte(nil), src.Hash...)
	dst.ID = src.ID
}

func cloneMembers(src []libraryAccount) []libraryAccount {
	if len(src) == 0 {
		return nil
	}
	dst := make([]libraryAccount, len(src))
	for i := range src {
		copyAccountFields(&dst[i], &src[i])
	}
	return dst
}

// cloneLocked returns an unshared deep copy of a. Caller must hold a.mu (R or W).
func (a *libraryAccount) cloneLocked() *libraryAccount {
	next := &libraryAccount{
		file:    a.file,
		Members: cloneMembers(a.Members),
	}
	copyAccountFields(next, a)
	return next
}

func openLibraryAccount(dir, initial string) (*libraryAccount, error) {
	a := &libraryAccount{file: filepath.Join(dir, "library-account.json")}
	data, err := os.ReadFile(a.file)
	if err == nil {
		if json.Unmarshal(data, a) != nil || a.Username == "" || len(a.Salt) != 16 || len(a.Hash) != 32 {
			return nil, errors.New("账号配置损坏，请从服务备份恢复")
		}
		return a, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if initial == "" {
		return nil, nil
	}
	a.Username = "teacher"
	return a, a.setPassword(initial)
}
func (a *libraryAccount) setPassword(password string) error {
	a.Salt = make([]byte, 16)
	if _, err := rand.Read(a.Salt); err != nil {
		return err
	}
	hash, err := pbkdf2.Key(sha256.New, password, a.Salt, 600000, 32)
	a.Hash = hash
	return err
}
func (a *libraryAccount) matches(password string) bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	salt := append([]byte(nil), a.Salt...)
	hash := append([]byte(nil), a.Hash...)
	a.mu.RUnlock()
	got, err := pbkdf2.Key(sha256.New, password, salt, 600000, 32)
	return err == nil && subtle.ConstantTimeCompare(got, hash) == 1
}
func (a *libraryAccount) findLocked(user string) *libraryAccount {
	if a.Username == user {
		return a
	}
	for i := range a.Members {
		if a.Members[i].Username == user {
			return &a.Members[i]
		}
	}
	return nil
}
func (a *libraryAccount) lookup(user string) (snap *libraryAccount, isAdmin bool, ok bool) {
	if a == nil {
		return nil, false, false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	m := a.findLocked(user)
	if m == nil {
		return nil, false, false
	}
	snap = &libraryAccount{}
	copyAccountFields(snap, m)
	return snap, m == a, true
}
func (a *libraryAccount) memberUsernames() []string {
	if a == nil {
		return []string{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	names := make([]string, len(a.Members))
	for i := range a.Members {
		names[i] = a.Members[i].Username
	}
	return names
}
func (a *libraryAccount) update(current, user, password string) error {
	return a.updateWithID(current, user, password, "")
}

func (a *libraryAccount) updateWithID(current, user, password, preferredID string) error {
	user = strings.TrimSpace(user)
	if user == "" || utf8.RuneCountInString(user) > 64 || strings.IndexFunc(user, unicode.IsControl) >= 0 {
		return errors.New("用户名须为 1 至 64 个字符，不能包含控制字符")
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()

	a.mu.RLock()
	if existing := a.findLocked(user); existing != nil && existing.Username != current {
		a.mu.RUnlock()
		return errors.New("用户名已使用")
	}
	next := a.cloneLocked()
	a.mu.RUnlock()

	var target *libraryAccount
	if current == "" {
		id := preferredID
		if id == "" || id == "admin" {
			id = rand.Text()
		}
		for i := range next.Members {
			if next.Members[i].ID == id {
				id = rand.Text()
				break
			}
		}
		next.Members = append(next.Members, libraryAccount{ID: id})
		target = &next.Members[len(next.Members)-1]
	} else {
		target = next.findLocked(current)
	}
	if target == nil {
		return errors.New("账号不存在")
	}
	target.Username = user
	if password != "" {
		if utf8.RuneCountInString(password) < 8 || len(password) > 1024 {
			return errors.New("新密码至少需要 8 个字符，最多 1024 字节")
		}
		if err := target.setPassword(password); err != nil {
			return errors.New("密码更新失败")
		}
	} else if current == "" {
		return errors.New("请填写初始密码")
	} else if user == current {
		return errors.New("请修改用户名或填写新密码")
	}
	return a.commit(next)
}

func (a *libraryAccount) memberIDMap() map[string]string {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.Members) == 0 {
		return nil
	}
	out := make(map[string]string, len(a.Members))
	for i := range a.Members {
		if a.Members[i].Username != "" && a.Members[i].ID != "" {
			out[a.Members[i].Username] = a.Members[i].ID
		}
	}
	return out
}

// commit writes the account file atomically, then makes next the live account.
// Caller must hold a.writeMu.
func (a *libraryAccount) commit(next *libraryAccount) error {
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(a.file), ".library-account-*")
	if err != nil {
		return errors.New("账号保存失败")
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil || os.Rename(f.Name(), a.file) != nil {
		return errors.New("账号保存失败")
	}
	a.mu.Lock()
	copyAccountFields(a, next)
	a.Members = cloneMembers(next.Members)
	a.mu.Unlock()
	return nil
}

// resetMember sets a new password for a teacher account. The administrator
// cannot reset their own password here; they use the account form instead.
func (a *libraryAccount) resetMember(user, password string) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()

	a.mu.RLock()
	if a.Username == user {
		a.mu.RUnlock()
		return errors.New("管理员请在账号设置中修改自己的密码")
	}
	if utf8.RuneCountInString(password) < 8 || len(password) > 1024 {
		a.mu.RUnlock()
		return errors.New("新密码至少需要 8 个字符，最多 1024 字节")
	}
	next := a.cloneLocked()
	a.mu.RUnlock()

	for i := range next.Members {
		if next.Members[i].Username == user {
			if err := next.Members[i].setPassword(password); err != nil {
				return errors.New("密码更新失败")
			}
			return a.commit(next)
		}
	}
	return errors.New("账号不存在")
}

// deleteMember removes a teacher account. The administrator account cannot be removed.
func (a *libraryAccount) deleteMember(user string) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()

	a.mu.RLock()
	if a.Username == user {
		a.mu.RUnlock()
		return errors.New("不能删除管理员账号")
	}
	next := a.cloneLocked()
	a.mu.RUnlock()

	filtered := make([]libraryAccount, 0, len(next.Members))
	found := false
	for i := range next.Members {
		if next.Members[i].Username == user {
			found = true
			continue
		}
		filtered = append(filtered, libraryAccount{})
		copyAccountFields(&filtered[len(filtered)-1], &next.Members[i])
	}
	if !found {
		return errors.New("账号不存在")
	}
	next.Members = filtered
	return a.commit(next)
}
