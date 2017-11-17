package debug

import (
	"fmt"
	ui "github.com/jroimartin/gocui"
	"strconv"
	"strings"
	"time"
)

const (
	HEADERS  = "headers"
	EVENTS   = "events"
	ROWS     = "rows"
	SELECTOR = "selector"
)

type DebugGui struct {
	*ui.Gui
	Path           string
	RowsGui        *RowsGui
	Columns        Columns
	ColumnSelector *ColumnSelector
	Events         *Events
}

func NewDebugGui(path string) (*DebugGui, error) {
	rows, err := RowsFromFile(path)
	if err != nil {
		return nil, err
	}

	g, err := ui.NewGui(ui.OutputNormal)
	if err != nil {
		return nil, err
	}

	columns := rows.AllColumns()

	dg := &DebugGui{
		Gui:     g,
		Path:    path,
		Columns: columns,
		Events:  &Events{},
	}
	dg.ColumnSelector = &ColumnSelector{DebugGui: dg}
	dg.RowsGui = &RowsGui{DebugGui: dg, Rows: rows}

	rows.SelectAll()

	dg.SetManager(dg.Events, dg.Columns, dg.RowsGui, dg.ColumnSelector)

	if err := dg.setKeybindings(); err != nil {
		return nil, err
	}

	dg.Events.Layout(g) // just to create it so the append will work.
	AppendEvent(g, fmt.Sprintf("Loaded %d rows from %s.", len(rows.All), path))

	return dg, nil
}

func (dg *DebugGui) setKeybindings() error {
	if err := dg.SetKeybinding("", 'q', ui.ModNone, quit); err != nil {
		return err
	} else if err := dg.SetKeybinding(HEADERS, ui.KeyArrowDown, ui.ModNone, dg.RowsGui.Down); err != nil {
		return err
	} else if err := dg.SetKeybinding(HEADERS, ui.KeyArrowUp, ui.ModNone, dg.RowsGui.Up); err != nil {
		return err
	} else if err := dg.SetKeybinding(HEADERS, ui.KeyPgdn, ui.ModNone, dg.RowsGui.PageDown); err != nil {
		return err
	} else if err := dg.SetKeybinding(HEADERS, ui.KeyPgup, ui.ModNone, dg.RowsGui.PageUp); err != nil {
		return err
	} else if err := dg.SetKeybinding(HEADERS, 'a', ui.ModNone, dg.RowsGui.All); err != nil {
		return err
	} else if err := dg.SetKeybinding(HEADERS, 'l', ui.ModNone, dg.RowsGui.Limit); err != nil {
		return err
	} else if err := dg.SetKeybinding(HEADERS, 's', ui.ModNone, dg.RowsGui.Search); err != nil {
		return err
	} else if err := dg.SetKeybinding(HEADERS, 'r', ui.ModNone, dg.RowsGui.SearchPrev); err != nil {
		return err
	} else if err := dg.SetKeybinding(HEADERS, ui.KeyEnter, ui.ModNone, dg.RowsGui.StopSearch); err != nil {
		return err
	} else if err := dg.SetKeybinding(HEADERS, ui.KeyArrowRight, ui.ModNone, dg.Columns.Right); err != nil {
		return err
	} else if err := dg.SetKeybinding(HEADERS, ui.KeyArrowLeft, ui.ModNone, dg.Columns.Left); err != nil {
		return err
	} else if err := dg.SetKeybinding(HEADERS, 'c', ui.ModNone, dg.ColumnSelector.Display); err != nil {
		return err
	} else if err := dg.SetKeybinding(SELECTOR, 'c', ui.ModNone, dg.ColumnSelector.Hide); err != nil {
		return err
	} else if err := dg.SetKeybinding(SELECTOR, 'h', ui.ModNone, dg.ColumnSelector.HideEmpty); err != nil {
		return err
	} else if err := dg.SetKeybinding(SELECTOR, 'a', ui.ModNone, dg.ColumnSelector.ShowAll); err != nil {
		return err
	} else if err := dg.SetKeybinding(SELECTOR, ui.KeyArrowDown, ui.ModNone, dg.ColumnSelector.CursorDown); err != nil {
		return err
	} else if err := dg.SetKeybinding(SELECTOR, ui.KeyArrowUp, ui.ModNone, dg.ColumnSelector.CursorUp); err != nil {
		return err
	} else if err := dg.SetKeybinding(SELECTOR, ui.KeyPgdn, ui.ModNone, dg.ColumnSelector.MoveDown); err != nil {
		return err
	} else if err := dg.SetKeybinding(SELECTOR, ui.KeyPgup, ui.ModNone, dg.ColumnSelector.MoveUp); err != nil {
		return err
	}

	return nil
}

func quit(g *ui.Gui, v *ui.View) error {
	return ui.ErrQuit
}

type Column struct {
	Name      string
	Displayed bool
	Selected  bool
	Width     int
	XStart    int
}

type Columns []*Column

func (cs Columns) ToggleDisplayed(idx int) {
	c := cs[idx]
	c.Displayed = !c.Displayed
	if c.Selected {
		c.Selected = false
		// attempt to select the next displayed column on the right
		for _, right := range cs[idx+1:] {
			if right.Displayed {
				right.Selected = true
				return
			}
		}
		// failed going right, so now go left
		for idx--; idx >= 0; idx-- {
			left := cs[idx]
			if left.Displayed {
				left.Selected = true
				return
			}
		}
		// failed again, so just have rule you can't turn off everything!
		c.Displayed = true
		c.Selected = true
	}
}

func (cs Columns) Left(g *ui.Gui, v *ui.View) error {
	var left *Column
	for _, c := range cs {
		if !c.Displayed {
			continue
		}
		if left != nil && c.Selected {
			c.Selected = false
			left.Selected = true
			return nil
		}
		left = c
	}
	return nil
}

func (cs Columns) Right(g *ui.Gui, v *ui.View) error {
	var left *Column
	for _, c := range cs {
		if !c.Displayed {
			continue
		}
		if left != nil && left.Selected {
			left.Selected = false
			c.Selected = true
			return nil
		}
		left = c
	}
	return nil
}

func (cs Columns) Layout(g *ui.Gui) error {
	screenWidth, _ := g.Size()
	v, err := g.SetView(HEADERS, 0, 0, screenWidth-1, 2)
	if err != nil {
		if err == ui.ErrUnknownView {
			if _, err := g.SetCurrentView(HEADERS); err != nil {
				return err
			}
		} else {
			return err
		}
		v.Wrap = false
		v.Frame = true
		v.Title = "(c to select columns)"
	}

	viewWidth := screenWidth - 2
	ox, _ := v.Origin()

	v.Clear()
	x := 0
	for _, c := range cs {
		if !c.Displayed {
			continue
		}
		c.XStart = x
		x += c.Width
		label := c.Name
		labelWidth := c.Width - 1
		if l := len(label); l > labelWidth {
			label = label[:labelWidth]
		} else if l < labelWidth {
			suffix := strings.Repeat(" ", labelWidth-l)
			label += suffix
		}
		if c.Selected {
			fmt.Fprintf(v, "\033[1m%s\033[0m<", label)

			// We must ensure the selected column is on the screen.  It's
			// more important to get the lhs of the column on the screen
			// than the rhs.
			if viewWidth <= c.Width || ox >= c.XStart {
				// screen too small  || origin too high to get lhs of column
				ox = c.XStart
			} else if x-ox > viewWidth {
				// rhs of column is on the right of the screen

				// Given that we have enough space to fix the whole column
				// on the screen, we should make sure the rhs is on the
				// screen as well as the left.
				ox = x - viewWidth
			}
		} else {
			fmt.Fprintf(v, "%s ", label)
		}
	}

	if viewWidth >= (x-ox) && ox > 0 { // x-ox is from origin to end of columns
		// We have space left over. We should try reducing the
		// origin to fit more columns in.
		ox = x - viewWidth
		if ox < 0 {
			ox = 0
		}
	}
	return v.SetOrigin(ox, 0)
}

type ColumnSelector struct {
	*DebugGui
	Displayed bool
}

func (cs *ColumnSelector) Layout(g *ui.Gui) error {
	if !cs.Displayed {
		return nil
	}
	screenWidth, screenHeight := g.Size()

	columnCount := len(cs.Columns)
	desiredHeight := columnCount + 1
	desiredWidth := 0
	for _, c := range cs.Columns {
		if len(c.Name) > desiredWidth {
			desiredWidth = len(c.Name)
		}
	}
	columnCountWidth := len(fmt.Sprintf("%d", columnCount))
	desiredWidth += len(fmt.Sprintf(" %*d. [x]   ", columnCountWidth, columnCount))
	if desiredWidth >= screenWidth {
		desiredWidth = screenWidth - 1
	}
	if desiredHeight >= screenHeight {
		desiredHeight = screenHeight - 1
	}
	midX, midY := screenWidth/2, screenHeight/2
	midH, midW := desiredHeight/2, desiredWidth/2

	v, err := g.SetView(SELECTOR, midX-(desiredWidth-midW), midY-(desiredHeight-midH), midX+midW, midY+midH)
	if err != nil {
		if err != ui.ErrUnknownView {
			return err
		}
		v.Frame = true
		v.Title = "Select Columns"
		v.Editor = cs
		v.Editable = true
		if columnCount > 0 {
			v.SetCursor(columnCountWidth+4, 0)
		}
	}
	v.Clear()
	for idx, c := range cs.Columns {
		d := " "
		if c.Displayed {
			d = "X"
		}
		fmt.Fprintf(v, " %*d. [%s] %s\n", columnCountWidth, idx, d, c.Name)
	}
	if _, err := g.SetCurrentView(SELECTOR); err != nil {
		return err
	}
	return nil
}

func (cs *ColumnSelector) Display(g *ui.Gui, v *ui.View) error {
	g.Cursor = true
	cs.Displayed = true
	return nil
}

func (cs *ColumnSelector) Hide(g *ui.Gui, v *ui.View) error {
	g.Cursor = false
	cs.Displayed = false
	if err := g.DeleteView(SELECTOR); err != nil {
		return err
	}
	if _, err := g.SetCurrentView(HEADERS); err != nil {
		return err
	}
	return nil
}

func (cs *ColumnSelector) ShowAll(g *ui.Gui, v *ui.View) error {
	for _, col := range cs.Columns {
		col.Displayed = true
	}
	return AppendEvent(g, "Showing all columns.")
}

// this does not enable non-empty columns - only hides empty ones
func (cs *ColumnSelector) HideEmpty(g *ui.Gui, v *ui.View) error {
	zeros := make([]int, len(cs.Columns))
	keys := make(map[string]*int, len(cs.Columns))
	for idx, col := range cs.Columns {
		keys[col.Name] = &zeros[idx]
	}
	for _, row := range cs.RowsGui.Selected {
		for k := range row {
			cPtr := keys[k]
			*cPtr++
		}
	}
	var oldSelectedIdx int
	selectedNeeded := false
	for idx, col := range cs.Columns {
		if cPtr := keys[col.Name]; *cPtr == 0 {
			col.Displayed = false
			if col.Selected {
				col.Selected = false
				oldSelectedIdx = idx
			}
		} else if col.Displayed && selectedNeeded {
			col.Selected = true
			selectedNeeded = false
		}
	}
	if selectedNeeded {
		for idx := oldSelectedIdx - 1; idx >= 0; idx-- {
			col := cs.Columns[idx]
			if col.Displayed {
				col.Selected = true
				break
			}
		}
	}
	return AppendEvent(g, "Hidden empty columns.")
}

func (cs *ColumnSelector) CursorDown(g *ui.Gui, v *ui.View) error {
	cx, cy := v.Cursor()
	ox, oy := v.Origin()
	lineNum := cy + oy + 1
	if lineNum >= len(cs.Columns) {
		return nil
	}
	_, h := v.Size()
	if cy == h-1 {
		return v.SetOrigin(ox, oy+1)
	} else {
		v.SetCursor(cx, cy+1)
	}
	return nil
}

func (cs *ColumnSelector) CursorUp(g *ui.Gui, v *ui.View) error {
	cx, cy := v.Cursor()
	ox, oy := v.Origin()
	lineNum := cy + oy - 1
	if lineNum < 0 {
		return nil
	}
	if cy == 0 {
		return v.SetOrigin(ox, oy-1)
	} else {
		v.SetCursor(cx, cy-1)
	}
	return nil
}

func (cs *ColumnSelector) MoveDown(g *ui.Gui, v *ui.View) error {
	_, cy := v.Cursor()
	_, oy := v.Origin()
	lineNum := cy + oy
	if lineNum+1 >= len(cs.Columns) {
		return nil
	}
	cs.Columns[lineNum], cs.Columns[lineNum+1] = cs.Columns[lineNum+1], cs.Columns[lineNum]
	return cs.CursorDown(g, v)
}

func (cs *ColumnSelector) MoveUp(g *ui.Gui, v *ui.View) error {
	_, cy := v.Cursor()
	_, oy := v.Origin()
	lineNum := cy + oy
	if lineNum <= 0 {
		return nil
	}
	cs.Columns[lineNum], cs.Columns[lineNum-1] = cs.Columns[lineNum-1], cs.Columns[lineNum]
	return cs.CursorUp(g, v)
}

func (cs *ColumnSelector) Edit(v *ui.View, key ui.Key, ch rune, mod ui.Modifier) {
	if '0' <= ch && ch <= '9' {
		if idx, err := strconv.Atoi(string(ch)); err == nil && idx < len(cs.Columns) {
			cs.Columns.ToggleDisplayed(idx)
		}
	} else if key == ui.KeySpace {
		_, cy := v.Cursor()
		_, oy := v.Origin()
		lineNum := cy + oy
		if lineNum < len(cs.Columns) {
			cs.Columns.ToggleDisplayed(lineNum)
		}
	}
}

type Events struct{}

func (e Events) Layout(g *ui.Gui) error {
	screenWidth, screenHeight := g.Size()
	v, err := g.SetView(EVENTS, 0, screenHeight-10, screenWidth-1, screenHeight-1)
	if err != nil {
		if err != ui.ErrUnknownView {
			return err
		}
		v.Wrap = true
		v.Autoscroll = true
		v.Frame = true
		v.Title = "Events"
	}
	return nil
}

func AppendEvent(g *ui.Gui, msg string) error {
	ev, err := g.View(EVENTS)
	if err != nil {
		return err
	}
	fmt.Fprintf(ev, "%30.30v: %s\n", time.Now().Format(time.RFC3339Nano), msg)
	return nil
}

type RowsGui struct {
	*Rows
	*DebugGui
	from      int
	highlight int
}

func (rg *RowsGui) Layout(g *ui.Gui) error {
	screenWidth, screenHeight := g.Size()
	v, err := g.SetView(ROWS, 0, 2, screenWidth-1, screenHeight-10)
	if err != nil {
		if err != ui.ErrUnknownView {
			return err
		}
		v.Frame = false
	}
	v.Clear()
	height := screenHeight - 10 - 3
	rg.Format(v, rg.Columns, rg.from, height, rg.highlight)

	headers, err := g.View(HEADERS)
	if err != nil {
		return err
	}
	ox, _ := headers.Origin()
	return v.SetOrigin(ox, 0)
}

func (rg *RowsGui) Down(g *ui.Gui, v *ui.View) error {
	v, err := g.View(ROWS)
	if err != nil {
		return err
	}
	rg.highlight++
	_, height := v.Size()
	to := height + rg.from
	if to > len(rg.Selected) {
		to = len(rg.Selected)
	}
	if rg.highlight >= to {
		if to < len(rg.Selected) {
			rg.from++
		} else {
			rg.highlight--
		}
	}
	return nil
}

func (rg *RowsGui) Up(g *ui.Gui, v *ui.View) error {
	rg.highlight--
	if rg.highlight < rg.from {
		if rg.highlight >= 0 {
			rg.from--
		} else {
			rg.highlight++
		}
	}
	return nil
}

func (rg *RowsGui) PageDown(g *ui.Gui, v *ui.View) error {
	v, err := g.View(ROWS)
	if err != nil {
		return err
	}
	_, height := v.Size()
	height--
	height /= 2
	if height == 0 {
		height = 1
	}
	for ; height > 0; height-- {
		if err := rg.Down(g, v); err != nil {
			return err
		}
	}
	return nil
}

func (rg *RowsGui) PageUp(g *ui.Gui, v *ui.View) error {
	v, err := g.View(ROWS)
	if err != nil {
		return err
	}
	_, height := v.Size()
	height--
	height /= 2
	if height == 0 {
		height = 1
	}
	for ; height > 0; height-- {
		if err := rg.Up(g, v); err != nil {
			return err
		}
	}
	return nil
}

func (rg *RowsGui) Limit(g *ui.Gui, v *ui.View) error {
	key := ""
	for _, c := range rg.Columns {
		if c.Selected {
			key = c.Name
			break
		}
	}
	row := rg.Selected[rg.highlight]
	val, found := row[key]
	if found && len(val) > 0 {
		screenRow := rg.highlight - rg.from
		rg.LimitSelected(key, val)
		// we want to try to keep the current row in the same place on
		// the screen.
		for idx, r := range rg.Selected {
			if r[TS] == row[TS] {
				if screenRow > idx {
					screenRow = idx
				}
				rg.from = idx - screenRow
				rg.highlight = idx
				break
			}
		}
		return AppendEvent(g, fmt.Sprintf("Added constraint %s=%s. %d matching rows", key, val, len(rg.Selected)))
	}
	return nil
}

func (rg *RowsGui) All(g *ui.Gui, v *ui.View) error {
	v, err := g.View(ROWS)
	if err != nil {
		return err
	}
	_, height := v.Size()
	// again, we try to keep the same row in the same place on the screen
	row := rg.Selected[rg.highlight]
	screenRow := rg.highlight - rg.from
	screenRowBotUp := height - screenRow
	rg.SelectAll()
	for idx, r := range rg.Selected {
		if r[TS] == row[TS] {
			if screenRowBotUp >= len(rg.Selected)-idx {
				screenRow = height - (len(rg.Selected) - idx)
			}
			rg.highlight = idx
			rg.from = idx - screenRow
			break
		}
	}
	return AppendEvent(g, fmt.Sprintf("Removed all constraints. %d rows.", len(rg.Selected)))
}

func (rg *RowsGui) Search(g *ui.Gui, v *ui.View) error {
	if len(rg.MatchingKey) == 0 {
		key := ""
		for _, c := range rg.Columns {
			if c.Selected {
				key = c.Name
				break
			}
		}
		row := rg.Selected[rg.highlight]
		val, found := row[key]
		if found && len(val) > 0 {
			rg.SetMatch(key, val)
			return AppendEvent(g, fmt.Sprintf("Highlighting %s=%s.", key, val))
		}
		return nil
	} else {
		v, err := g.View(ROWS)
		if err != nil {
			return err
		}
		_, height := v.Size()
		old := rg.highlight
		rg.highlight = rg.NextMatch(rg.highlight, true)
		if rg.highlight >= rg.from+height {
			// we should re-center the screen
			rg.from = rg.highlight - (height / 2)
			if rg.from+height > len(rg.Selected) {
				rg.from = len(rg.Selected) - height
			}
		}
		if old == rg.highlight {
			return AppendEvent(g, fmt.Sprintf("No further matches found."))
		}
		return nil
	}
}

func (rg *RowsGui) SearchPrev(g *ui.Gui, v *ui.View) error {
	if len(rg.MatchingKey) == 0 {
		key := ""
		for _, c := range rg.Columns {
			if c.Selected {
				key = c.Name
				break
			}
		}
		row := rg.Selected[rg.highlight]
		val, found := row[key]
		if found && len(val) > 0 {
			rg.SetMatch(key, val)
			return AppendEvent(g, fmt.Sprintf("Highlighting %s=%s.", key, val))
		}
		return nil
	} else {
		v, err := g.View(ROWS)
		if err != nil {
			return err
		}
		_, height := v.Size()
		old := rg.highlight
		rg.highlight = rg.NextMatch(rg.highlight, false)
		if rg.highlight < rg.from {
			// we should re-center the screen
			rg.from = rg.highlight - (height / 2)
			if rg.from < 0 {
				rg.from = 0
			}
		}
		if old == rg.highlight {
			return AppendEvent(g, fmt.Sprintf("No further matches found."))
		}
		return nil
	}
}

func (rg *RowsGui) StopSearch(g *ui.Gui, v *ui.View) error {
	rg.SetMatch("", "")
	return AppendEvent(g, fmt.Sprintf("Cleared Highlighting."))
}