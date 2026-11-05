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
	hash, err := pbkdf2.Key(sha256.New, password, a.Salt, 600000, 32)
	return err == nil && subtle.ConstantTimeCompare(hash, a.Hash) == 1
}
func (a *libraryAccount) find(user string) *libraryAccount {
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
func (a *libraryAccount) update(current, user, password string) error {
	user = strings.TrimSpace(user)
	if user == "" || utf8.RuneCountInString(user) > 64 || strings.IndexFunc(user, unicode.IsControl) >= 0 {
		return errors.New("用户名须为 1 至 64 个字符，不能包含控制字符")
	}
	if existing := a.find(user); existing != nil && existing.Username != current {
		return errors.New("用户名已使用")
	}
	next := *a
	next.Members = append([]libraryAccount(nil), a.Members...)
	var target *libraryAccount
	if current == "" {
		next.Members = append(next.Members, libraryAccount{ID: rand.Text()})
		target = &next.Members[len(next.Members)-1]
	} else {
		target = next.find(current)
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
	} else if current == "" || user == current {
		return errors.New("请修改用户名或填写新密码")
	}
	return a.commit(next)
}

// commit writes the account file atomically, then makes next the live account.
func (a *libraryAccount) commit(next libraryAccount) error {
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
	*a = next
	return nil
}

// resetMember sets a new password for a teacher account. The administrator
// cannot reset their own password here; they use the account form instead.
func (a *libraryAccount) resetMember(user, password string) error {
	if a.Username == user {
		return errors.New("管理员请在账号设置中修改自己的密码")
	}
	if utf8.RuneCountInString(password) < 8 || len(password) > 1024 {
		return errors.New("新密码至少需要 8 个字符，最多 1024 字节")
	}
	next := *a
	next.Members = append([]libraryAccount(nil), a.Members...)
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
	if a.Username == user {
		return errors.New("不能删除管理员账号")
	}
	next := *a
	next.Members = nil
	found := false
	for _, m := range a.Members {
		if m.Username == user {
			found = true
			continue
		}
		next.Members = append(next.Members, m)
	}
	if !found {
		return errors.New("账号不存在")
	}
	return a.commit(next)
}
