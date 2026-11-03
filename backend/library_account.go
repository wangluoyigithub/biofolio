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
	Username string `json:"username"`
	Salt     []byte `json:"salt"`
	Hash     []byte `json:"pbkdf2_sha256_600000"`
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
func (a *libraryAccount) change(user, password string) error {
	user = strings.TrimSpace(user)
	if user == "" || utf8.RuneCountInString(user) > 64 || strings.IndexFunc(user, unicode.IsControl) >= 0 {
		return errors.New("用户名须为 1 至 64 个字符，不能包含控制字符")
	}
	next := *a
	next.Username = user
	if password != "" {
		if utf8.RuneCountInString(password) < 8 || len(password) > 1024 {
			return errors.New("新密码至少需要 8 个字符，最多 1024 字节")
		}
		if err := next.setPassword(password); err != nil {
			return errors.New("密码更新失败")
		}
	} else if user == a.Username {
		return errors.New("请修改用户名或填写新密码")
	}
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
