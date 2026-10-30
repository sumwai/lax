package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// ErrModelAliasNotFound 表示库里没有这个别名。`model alias unset` 用 errors.Is 判它，
// 把「别名写错了」与「数据库打不开」分开：删不到一律算失败，不静默当成功。
var ErrModelAliasNotFound = errors.New("别名不存在")

// ErrModelAliasShadow 表示要写的别名已经是真实模型名，见 DESIGN 5.12。
// 真名有两处：目录（provider_models）里有这一行，或者某个账号现在就能用它（account_models
// 里 access='allowed'）。这种别名会遮蔽真名：客户端请求那个名字时，命中哪个模型会随账号增减
// 而变，所以写入侧直接拒绝，而不是留到请求时才发现。
var ErrModelAliasShadow = errors.New("该名字是真实模型名，别名会遮蔽它")

// ErrModelAliasCycle 表示别名链绕回了链上已经出现过的名字。成环的链永远解析不出真实名字。
var ErrModelAliasCycle = errors.New("别名链成环")

// ErrModelAliasTooDeep 表示别名链超过 ModelAliasMaxHops 跳还没走到真实名字。
// 链过长多半是别名互相串起来写错了：与其让请求时悄悄多走几跳，不如解析时就报出来。
var ErrModelAliasTooDeep = errors.New("别名链太长")

// ModelAliasMaxHops 是一条别名链最多能走几跳（DESIGN 5.12）。
// 一跳是「别名 → 真实名字」；八跳够把几个客户端的写死名字串到同一个真名上，再多只能说明链写歪了。
const ModelAliasMaxHops = 8

// ModelAlias 是 model_aliases 里的一行（迁移 0013，见 DESIGN 5.12、6）。
// Alias 是客户端用的名字，Target 是真实模型名或另一个别名，CreatedAt 是这一行第一次写进来的
// Unix 秒（覆盖改目标不动它）。这里没有凭据：别名表只放名字。
type ModelAlias struct {
	Alias     string
	Target    string
	CreatedAt int64
}

// SetModelAlias 写入（或覆盖）一条别名，是这张表的唯一写入口。
// 同一个别名再写一次就是改目标，不报错、不留旧行；覆盖时不改 created_at。
//
// 校验按 DESIGN 5.12：别名与目标都去首尾空白后不能为空（落库就是去空白后的值）、不能含控制
// 字符、别名不能等于目标，别名也不能与任何真实模型名同名——那会遮蔽真名。真名按两处认：
// 目录（provider_models）里存在同名模型，或者某个账号现在就能用它（access='allowed'）。
// 只查账号会漏掉「目录里有、眼下没有账号可用」的名字：账号一恢复可用，这个别名的行为
// 就会突然改变，而那时再报错已经晚于别名写进库。
// 目标可以暂时没有任何账号提供：先把别名写好，之后再补账号就能生效。
// 校验与写入在同一个事务里，判定冲突与落库之间不会插进一条新账号或一条新目录行。
func (s *Store) SetModelAlias(alias, target string) error {
	a, err := normalizeAliasName(alias, "别名")
	if err != nil {
		return err
	}
	t, err := normalizeAliasName(target, "目标")
	if err != nil {
		return fmt.Errorf("别名 %q：%w", a, err)
	}
	if a == t {
		return fmt.Errorf("别名 %q 与目标相同：别名要指向一个真实模型名，同名不会带来任何解析，还会挡住以后给这个名字加别名", a)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	providers, err := aliasCatalogProvidersTx(tx, a)
	if err != nil {
		return err
	}
	accounts, err := aliasShadowingAccountsTx(tx, a)
	if err != nil {
		return err
	}
	if len(providers) > 0 || len(accounts) > 0 {
		return aliasShadowErr(a, providers, accounts)
	}

	if _, err := tx.Exec(`INSERT INTO model_aliases (alias, target, created_at) VALUES (?, ?, ?)
		ON CONFLICT (alias) DO UPDATE SET target = excluded.target`,
		a, t, time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteModelAlias 删掉一条别名，是这张表的唯一删除入口。
// 删不到时返回 ErrModelAliasNotFound：别名可能从来没写过，也可能已经被删掉。
func (s *Store) DeleteModelAlias(alias string) error {
	a, err := normalizeAliasName(alias, "别名")
	if err != nil {
		return err
	}
	res, err := s.db.Exec(`DELETE FROM model_aliases WHERE alias = ?`, a)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w：%q，用 lax model alias ls 看已配置的别名", ErrModelAliasNotFound, a)
	}
	return nil
}

// ModelAliases 按别名升序返回全部别名（SQLite 的 TEXT 默认二进制序，也就是区分大小写）。
// 表是空的就返回空切片，不算错误：一条别名都没有是正常状态，请求全按真实名字走。
func (s *Store) ModelAliases() ([]ModelAlias, error) {
	rows, err := s.db.Query(`SELECT alias, target, created_at FROM model_aliases ORDER BY alias`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ModelAlias{}
	for rows.Next() {
		var m ModelAlias
		if err := rows.Scan(&m.Alias, &m.Target, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ResolveModelAlias 把入站模型名解析成真实模型名，是走链的唯一入口：
// 调用方只问一次，不自己拼多跳。名字不是别名时返回 ("", false, nil)；是别名时沿链走到
// 第一个不是别名的名字，返回 (真实名字, true, nil)。链成环返回 ErrModelAliasCycle、
// 超过 ModelAliasMaxHops 跳返回 ErrModelAliasTooDeep，两种都带上链上的名字，方便看出是哪几条写歪了。
//
// 表很小（别名是手工维护的），一次读出全表比按跳查库省事：一趟查询、不重不漏，也不会出现
// 链走到一半表被改掉、前半段用旧目标后半段用新目标。
func (s *Store) ResolveModelAlias(name string) (string, bool, error) {
	targets, err := aliasTargets(s.db)
	if err != nil {
		return "", false, err
	}
	return resolveAliasChain(targets, name)
}

// aliasTargets 读出「别名 → 目标」全表，解析与成环判定都在内存里做。
func aliasTargets(db *sql.DB) (map[string]string, error) {
	rows, err := db.Query(`SELECT alias, target FROM model_aliases`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	targets := map[string]string{}
	for rows.Next() {
		var alias, target string
		if err := rows.Scan(&alias, &target); err != nil {
			return nil, err
		}
		targets[alias] = target
	}
	return targets, rows.Err()
}

// resolveAliasChain 是 ResolveModelAlias 的纯逻辑部分：输入全表的「别名 → 目标」，输出解析结果。
// chain 记下沿路走过的名字，成环与超长都把这条链原文写进错误，用户照着它就能找到写歪的那几条。
func resolveAliasChain(targets map[string]string, name string) (string, bool, error) {
	if _, ok := targets[name]; !ok {
		return "", false, nil
	}
	chain := []string{name}
	cur := name
	// hops 是已经走过的跳数；chain[i] 是第 i 跳上的名字（chain[0] 是入站名字，0 跳）。
	for hops := 0; ; hops++ {
		next, ok := targets[cur]
		if !ok {
			return cur, true, nil
		}
		// 目标回到链上已经出现过的名字就是环：从它第一次出现的位置截到链尾，再接上它自己。
		for i, seen := range chain {
			if seen == next {
				loop := strings.Join(chain[i:], " → ") + " → " + next
				return "", false, fmt.Errorf("%w：%s；删掉或改掉其中一条就能解开，"+
					"别名链最长 %d 跳", ErrModelAliasCycle, loop, ModelAliasMaxHops)
			}
		}
		// 这一跳落到 next 上。next 也是别名时，走完它至少还要一跳，所以至少用掉 hops+2 跳；
		// 超过上限就报出来，不把长链拖到请求时才炸。
		if _, continues := targets[next]; continues && hops+2 > ModelAliasMaxHops {
			return "", false, fmt.Errorf("%w：%s → %s 已经到第 %d 跳，它还是别名，走到真实模型名至少要 %d 跳；"+
				"把中间几跳直接指到真名上，链最长 %d 跳",
				ErrModelAliasTooDeep, strings.Join(chain, " → "), next, len(chain), len(chain)+1, ModelAliasMaxHops)
		}
		chain = append(chain, next)
		cur = next
	}
}

// aliasCatalogProvidersTx 在事务里找出目录里有 alias 这个模型的厂商，按名称升序。
// 目录（provider_models）是真名的事实来源（DESIGN 5.13）：一家厂商能看到的模型与「眼下有没有
// 账号能用它」是两件事，实测 bigmodel 的清单里就有某个 key 无权访问的模型。目录里的名字因此
// 与当前可达性无关，仍然是会被别名遮蔽的真名。
func aliasCatalogProvidersTx(tx *sql.Tx, alias string) ([]string, error) {
	rows, err := tx.Query(`SELECT DISTINCT provider FROM provider_models
		WHERE model = ? ORDER BY provider`, alias)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hits []string
	for rows.Next() {
		var provider string
		if err := rows.Scan(&provider); err != nil {
			return nil, err
		}
		hits = append(hits, provider)
	}
	return hits, rows.Err()
}

// aliasShadowErr 把两处命中的结果合成一句话：目录与账号各说一半，两处都命中时也只报一次。
// 只有账号命中时文案与引入目录检查之前一致（写歪的别名多半是撞上正在用的模型，那句最贴近现场）；
// 只有目录命中时说明「眼下没人能用」——不然用户找不到一个声明了这个名字的账号。
func aliasShadowErr(alias string, providers, accounts []string) error {
	switch {
	case len(providers) > 0 && len(accounts) > 0:
		return fmt.Errorf("%w：%q 在 %s 的目录里是真实模型名，账号 %s 现在也在用它；"+
			"客户端请求那个名字时会跟着账号增减换模型，换个别名（例如 fast-%s）或改掉账号的 --models",
			ErrModelAliasShadow, alias, shadowNames(providers, "家厂商"), shadowNames(accounts, "个账号"), alias)
	case len(providers) > 0:
		return fmt.Errorf("%w：%q 在 %s 的目录里是真实模型名，眼下没有账号能用它；"+
			"等账号恢复可用时客户端请求这个名字就会换模型，换个别名（例如 fast-%s）或改名目录里那条",
			ErrModelAliasShadow, alias, shadowNames(providers, "家厂商"), alias)
	default:
		return fmt.Errorf("%w：%q 已被账号 %s 声明为真实模型，别名会遮蔽真名；"+
			"客户端请求那个名字时会跟着账号增减换模型，换个别名（例如 fast-%s）或改掉账号的 --models",
			ErrModelAliasShadow, alias, shadowNames(accounts, "个账号"), alias)
	}
}

// aliasShadowingAccountsTx 在事务里找出把 alias 声明成真实模型名、且现在就能用的账号，按名称升序。
// 比的是该账号 access='allowed' 的模型名集合——迁移 0015 之后它就是「这个账号现在能用哪些
// 真实模型名」的那份清单（原来是 accounts.models 那一列 JSON），区分大小写的精确匹配（DESIGN 5.12）：
// 名字发给上游时不会做大小写归一，`GLM-4.6` 与 `glm-4.6` 在上游看来是两个名字。
func aliasShadowingAccountsTx(tx *sql.Tx, alias string) ([]string, error) {
	rows, err := tx.Query(`SELECT DISTINCT a.name FROM accounts a
		JOIN account_models m ON m.account_id = a.id
		WHERE m.access = ? AND m.model = ? ORDER BY a.name`, AccessAllowed, alias)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hits []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		hits = append(hits, name)
	}
	return hits, rows.Err()
}

// shadowNames 把一组名字列成文案里的一截：最多列三个，其余的只报个数，
// 免得一个名字被二十个账号声明时错误信息变成一屏名字。unit 是数量词（「个账号」「家厂商」），
// 目录与账号两处共用它，截断规则就只有一份。
func shadowNames(names []string, unit string) string {
	if len(names) <= 3 {
		return strings.Join(names, "、")
	}
	return strings.Join(names[:3], "、") + fmt.Sprintf(" 等 %d %s", len(names), unit)
}

// normalizeAliasName 校验一个模型名（别名或目标）并去掉首尾空白。
// 落库的是去空白之后的值：同一行里 `  gpt-4o ` 与 `gpt-4o` 若各存一份，看起来像两个名字，
// 却会解析到同一个目标。控制字符一律拒绝——名字会原样进请求体与 URL 路径，也能进日志与表格。
func normalizeAliasName(value, field string) (string, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return "", fmt.Errorf("%s不能为空", field)
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%s %q 里有控制字符（U+%04X）：模型名会原样发给上游，带控制字符的名字发不出去", field, v, r)
		}
	}
	return v, nil
}
