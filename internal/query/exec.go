package query

import (
	"lantern/internal/index"
	"lantern/internal/rank"
)

// DocIterator 是执行器的文档游标(规格 6 的接口)。
// 约定:Next/Advance 返回 true 后,DocID/Score 有效。
type DocIterator interface {
	// Next 前进到下一个匹配文档。
	Next() bool
	// Advance 定位到第一个 >= target 的匹配文档;耗尽返回 false。
	Advance(target uint32) bool
	DocID() uint32
	// Score 返回当前文档的本子树得分贡献(AND 为子节点之和,
	// 短语/过滤为 0,NOT 为 0)。
	Score() float64
}

// segEnv 是单段执行环境。
type segEnv struct {
	r      *index.SegmentReader
	params rank.Params
	// avg 为跨段汇总的全局平均字段长度(规格 7.1 跨段一致)。
	avg [index.NumFields]float64
}

// scoreTerm 是一个(可能展开的)词项的全局打分上下文。
type scoreTerm struct {
	text   string
	weight float64 // 展开权重 = 1 - dist/len;普通词项 = 1
	df     uint64  // 各字段 df 之和的最大值(跨段汇总)
	idf    float64
	fields []index.Field // 检索字段范围
	// dfByField 记录各字段的跨段 df 之和(Explain 用)。
	dfByField [index.NumFields]uint64
}

// scopedFields 返回检索字段范围。
func (sc *scoreTerm) scopedFields() []index.Field { return sc.fields }

// allIter 遍历段内全部存活文档。
// 约定:构造后未定位;首次 Next/Advance 定位到第一个匹配。
type allIter struct {
	se      *segEnv
	cur     uint32
	ok      bool
	started bool
	done    bool
}

func (it *allIter) Next() bool {
	if it.done {
		return false
	}
	if !it.started {
		it.started = true
		return it.Advance(0)
	}
	return it.Advance(it.cur + 1)
}

func (it *allIter) Advance(target uint32) bool {
	if it.done {
		return false
	}
	for d := target; d < it.se.r.DocCount(); d++ {
		if it.se.r.IsAlive(d) {
			it.cur = d
			it.ok = true
			return true
		}
	}
	it.done = true
	it.ok = false
	return false
}

func (it *allIter) DocID() uint32  { return it.cur }
func (it *allIter) Score() float64 { return 0 }

// termIter 是一个词项在单段内的多字段合并游标:
// 字段之间为 OR(任一字段含该词项即命中),tf 按字段记录。
type termIter struct {
	se     *segEnv
	sc     *scoreTerm
	fields []int                    // 字段编号
	its    []*index.PostingIterator // 与 fields 平行
	gone   []bool                   // 字段游标耗尽标记
	tf     [index.NumFields]uint32
	cur    uint32
	ok     bool
	done   bool
}

func newTermIter(se *segEnv, sc *scoreTerm, fields []index.Field) *termIter {
	t := &termIter{se: se, sc: sc}
	for _, f := range fields {
		it, err := se.r.Postings(f, sc.text)
		if err != nil || it == nil {
			continue
		}
		t.fields = append(t.fields, int(f))
		t.its = append(t.its, it)
		t.gone = append(t.gone, false)
	}
	if len(t.its) == 0 {
		t.done = true
	}
	return t
}

// sync 定位到各字段游标中的最小 docID(字段之间 OR),记录该文档
// 各字段 tf;位于该文档的字段游标原地保留供 Score 使用。
func (t *termIter) sync() bool {
	found := false
	var min uint32
	for i, it := range t.its {
		if t.gone[i] {
			continue
		}
		d := it.DocID()
		if !found || d < min {
			min = d
			found = true
		}
	}
	if !found {
		t.done = true
		t.ok = false
		return false
	}
	for i := range t.its {
		t.tf[t.fields[i]] = 0
		if !t.gone[i] && t.its[i].DocID() == min {
			t.tf[t.fields[i]] = t.its[i].TF()
		}
	}
	t.cur = min
	t.ok = true
	return true
}

func (t *termIter) Next() bool {
	if t.done {
		return false
	}
	if !t.ok {
		for i, it := range t.its {
			if !it.Next() {
				t.gone[i] = true
			}
		}
		return t.sync()
	}
	// 只推进位于当前文档的字段。
	for i := range t.its {
		if !t.gone[i] && t.its[i].DocID() == t.cur {
			if !t.its[i].Next() {
				t.gone[i] = true
			}
		}
	}
	return t.sync()
}

func (t *termIter) Advance(target uint32) bool {
	if t.done {
		return false
	}
	if t.ok && t.cur >= target {
		return true
	}
	for i, it := range t.its {
		if t.gone[i] {
			continue
		}
		if !t.ok || it.DocID() < target {
			if !it.Advance(target) {
				t.gone[i] = true
			}
		}
	}
	return t.sync()
}

func (t *termIter) DocID() uint32 { return t.cur }

// Score 计算该词项对当前文档的贡献:
// 先跨字段求和 tfw,再做一次 idf·tfw/(k1+tfw)(BM25F)。
func (t *termIter) Score() float64 {
	if !t.ok {
		return 0
	}
	var tf, ln [index.NumFields]float64
	for i, f := range t.fields {
		tf[f] = float64(t.tf[t.fields[i]])
		if n, err := t.se.r.Norm(t.cur, index.Field(f)); err == nil {
			ln[f] = float64(n)
		}
	}
	tfw := rank.TFW(t.se.params, tf, ln, t.se.avg)
	return rank.TermScore(t.se.params, t.sc.idf, tfw, t.sc.weight)
}

// Fields 返回当前文档各字段 tf(Explain 用)。
func (t *termIter) FieldTF() [index.NumFields]uint32 { return t.tf }

// wrap 记录子树是否耗尽(or 归并用)。
type wrap struct {
	it   DocIterator
	done bool
}

// orIter 析取:多路归并,同文档贡献求和。
type orIter struct {
	kids []*wrap
	cur  uint32
	ok   bool
	done bool
}

func newOrIter(kids []DocIterator) DocIterator {
	live := make([]*wrap, 0, len(kids))
	for _, k := range kids {
		if k != nil {
			live = append(live, &wrap{it: k})
		}
	}
	if len(live) == 0 {
		return nil
	}
	if len(live) == 1 {
		return live[0].it
	}
	o := &orIter{kids: live}
	return o
}

// init 首次定位各子游标(惰性,保持 Next 首调语义)。
func (o *orIter) init(target uint32, useTarget bool) bool {
	for _, w := range o.kids {
		var ok bool
		if useTarget {
			ok = w.it.Advance(target)
		} else {
			ok = w.it.Next()
		}
		if !ok {
			w.done = true
		}
	}
	return o.recompute()
}

func (o *orIter) recompute() bool {
	found := false
	var min uint32
	for _, w := range o.kids {
		if w.done {
			continue
		}
		d := w.it.DocID()
		if !found || d < min {
			min = d
			found = true
		}
	}
	if !found {
		o.done = true
		o.ok = false
		return false
	}
	o.cur = min
	o.ok = true
	return true
}

func (o *orIter) Next() bool {
	if o.done {
		return false
	}
	if !o.ok {
		if o.initialized() {
			return false
		}
		return o.init(0, false)
	}
	for _, w := range o.kids {
		if !w.done && w.it.DocID() == o.cur {
			if !w.it.Next() {
				w.done = true
			}
		}
	}
	return o.recompute()
}

func (o *orIter) initialized() bool {
	for _, w := range o.kids {
		if !w.done {
			return false
		}
	}
	return true
}

func (o *orIter) Advance(target uint32) bool {
	if o.done {
		return false
	}
	if o.ok && o.cur >= target {
		return true
	}
	anyLive := false
	for _, w := range o.kids {
		if w.done {
			continue
		}
		anyLive = true
		if !o.ok || w.it.DocID() < target {
			if !w.it.Advance(target) {
				w.done = true
			}
		}
	}
	if !anyLive {
		o.done = true
		o.ok = false
		return false
	}
	return o.recompute()
}

func (o *orIter) DocID() uint32 { return o.cur }

func (o *orIter) Score() float64 {
	if !o.ok {
		return 0
	}
	s := 0.0
	for _, w := range o.kids {
		if !w.done && w.it.DocID() == o.cur {
			s += w.it.Score()
		}
	}
	return s
}

// andIter 合取(leapfrog + skip):
// pos 为正向来源;negs 为排除子树(NOT);filters 为 doc values 条件
// (惰性求值:仅对候选文档调用)。
type andIter struct {
	pos     []*wrap
	negs    []*wrap
	filters []*filterCond
	se      *segEnv
	cur     uint32
	ok      bool
	done    bool
}

func newAndIter(pos []DocIterator, negs []DocIterator, filters []*filterCond, se *segEnv) DocIterator {
	a := &andIter{filters: filters, se: se}
	for _, p := range pos {
		if p != nil {
			a.pos = append(a.pos, &wrap{it: p})
		}
	}
	for _, n := range negs {
		if n != nil {
			a.negs = append(a.negs, &wrap{it: n})
		}
	}
	if len(a.pos) == 0 {
		// 纯否定/纯过滤:以全量游标为驱动源。
		a.pos = append(a.pos, &wrap{it: &allIter{se: se}})
	}
	// 惰性:不在此处定位,首次 Next/Advance 再初始化。
	return a
}

// init 首次定位全部正向来源。
func (a *andIter) init(target uint32, useTarget bool) bool {
	for _, w := range a.pos {
		var ok bool
		if useTarget {
			ok = w.it.Advance(target)
		} else {
			ok = w.it.Next()
		}
		if !ok {
			w.done = true
			a.done = true
			return false
		}
	}
	return true
}

// findMatch 从当前位置出发,寻找满足全部否定与过滤条件的公共文档。
func (a *andIter) findMatch() bool {
	for {
		if a.done {
			return false
		}
		// leapfrog:候选 = 最大 DocID;依次推进未达候选的来源。
		candidate := a.pos[0].it.DocID()
		for _, w := range a.pos {
			if w.it.DocID() > candidate {
				candidate = w.it.DocID()
			}
		}
		i := 0
		for {
			w := a.pos[i]
			if w.it.DocID() < candidate {
				if !w.it.Advance(candidate) {
					a.done = true
					a.ok = false
					return false
				}
				if d := w.it.DocID(); d > candidate {
					candidate = d
					i = 0
					continue
				}
			}
			i++
			if i == len(a.pos) {
				break
			}
		}
		// 否定条件:任何 neg 命中候选则跳过。
		rejected := false
		for _, n := range a.negs {
			if n.done {
				continue
			}
			if !n.it.Advance(candidate) {
				n.done = true
				continue
			}
			if n.it.DocID() == candidate {
				rejected = true
				break
			}
		}
		if !rejected {
			for _, fc := range a.filters {
				if !fc.match(a.se.r, candidate) {
					rejected = true
					break
				}
			}
		}
		if rejected {
			// 推进最小来源继续。
			if !a.pos[0].it.Advance(candidate + 1) {
				a.done = true
				a.ok = false
				return false
			}
			continue
		}
		a.cur = candidate
		a.ok = true
		return true
	}
}

func (a *andIter) Next() bool {
	if a.done {
		return false
	}
	if !a.ok {
		if a.pos[0].done {
			return false
		}
		if !a.init(0, false) {
			return false
		}
		return a.findMatch()
	}
	if !a.pos[0].it.Advance(a.cur + 1) {
		a.done = true
		a.ok = false
		return false
	}
	return a.findMatch()
}

func (a *andIter) Advance(target uint32) bool {
	if a.done {
		return false
	}
	if a.ok && a.cur >= target {
		return true
	}
	if !a.pos[0].done && !a.ok {
		if !a.init(target, true) {
			return false
		}
		return a.findMatch()
	}
	if !a.pos[0].it.Advance(target) {
		a.done = true
		a.ok = false
		return false
	}
	return a.findMatch()
}

func (a *andIter) DocID() uint32 { return a.cur }

func (a *andIter) Score() float64 {
	if !a.ok {
		return 0
	}
	s := 0.0
	for _, w := range a.pos {
		if !w.done && w.it.DocID() == a.cur {
			s += w.it.Score()
		}
	}
	return s
}

// phraseIter 是短语/邻近游标(单字段单段)。
// Slop=0:严格短语(顺序且相邻);Slop>0:邻近(任意顺序,
// 窗口 = Slop + n - 1)。
type phraseIter struct {
	its         []*index.PostingIterator // 各词项(同字段)
	slop        int
	cur         uint32
	ok          bool
	done        bool
	spanScratch []posTerm
}

type posTerm struct {
	pos  uint32
	term int
}

func newPhraseIter(its []*index.PostingIterator, slop int) *phraseIter {
	if len(its) == 0 {
		return &phraseIter{done: true}
	}
	return &phraseIter{its: its, slop: slop}
}

// syncDoc 把所有词项游标同步到同一文档。
func (p *phraseIter) syncDoc(candidate uint32) bool {
	for {
		maxD := uint32(0)
		for _, it := range p.its {
			if !it.Advance(candidate) {
				p.done = true
				return false
			}
			if it.DocID() > maxD {
				maxD = it.DocID()
			}
		}
		if maxD == candidate {
			return true
		}
		candidate = maxD
	}
}

// matchPositions 检查当前公共文档的位置是否满足短语/邻近。
func (p *phraseIter) matchPositions() bool {
	n := len(p.its)
	if n == 1 {
		// 单词"短语"退化为词项存在。
		return true
	}
	if p.slop <= 0 {
		// 严格:存在锚点 p 使词项 i 命中 p+i。
		for _, anchor := range p.its[0].Positions() {
			ok := true
			for i := 1; i < n; i++ {
				if !posContains(p.its[i].Positions(), anchor+uint32(i)) {
					ok = false
					break
				}
			}
			if ok {
				return true
			}
		}
		return false
	}
	// 邻近:任意顺序,最小窗口 <= slop + n - 1。
	limit := uint32(p.slop) + uint32(n) - 1
	p.spanScratch = p.spanScratch[:0]
	for i, it := range p.its {
		for _, pos := range it.Positions() {
			p.spanScratch = append(p.spanScratch, posTerm{pos, i})
		}
	}
	sortPosTerms(p.spanScratch)
	need := make([]int, n)
	missing := n
	best := uint32(1<<31 - 1)
	lo := 0
	for hi := 0; hi < len(p.spanScratch); hi++ {
		e := p.spanScratch[hi]
		need[e.term]++
		if need[e.term] == 1 {
			missing--
		}
		for missing == 0 {
			span := p.spanScratch[hi].pos - p.spanScratch[lo].pos
			if span < best {
				best = span
			}
			le := p.spanScratch[lo]
			need[le.term]--
			if need[le.term] == 0 {
				missing++
			}
			lo++
		}
	}
	return best <= limit
}

func posContains(sorted []uint32, v uint32) bool {
	lo, hi := 0, len(sorted)-1
	for lo <= hi {
		mid := int(uint(lo+hi) >> 1)
		if sorted[mid] == v {
			return true
		}
		if sorted[mid] < v {
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return false
}

func sortPosTerms(a []posTerm) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j].pos < a[j-1].pos; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

func (p *phraseIter) Next() bool {
	if p.done {
		return false
	}
	if !p.ok {
		return p.findFrom(0)
	}
	return p.findFrom(p.cur + 1)
}

func (p *phraseIter) Advance(target uint32) bool {
	if p.done {
		return false
	}
	if p.ok && p.cur >= target {
		return true
	}
	return p.findFrom(target)
}

func (p *phraseIter) findFrom(target uint32) bool {
	for {
		if !p.syncDoc(target) {
			return false
		}
		p.cur = p.its[0].DocID()
		p.ok = true
		if p.matchPositions() {
			return true
		}
		target = p.cur + 1
	}
}

func (p *phraseIter) DocID() uint32  { return p.cur }
func (p *phraseIter) Score() float64 { return 0 }
