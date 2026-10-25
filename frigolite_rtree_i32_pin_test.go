package frigolite

import (
	"fmt"
	"testing"
)

// rtree2.test's rtree_i32 sections compare `SELECT * FROM t1 WHERE <preds>`
// on the virtual table against the same predicates on a mirror table and
// require identical row sets (102 failing leaves at run 31 all say the
// comparison fails). This probe replays the OPS mixes the test generates
// (ii and cN columns with < > <= >= =) on a deterministic row set and
// reports the first diverging predicate against the mirror.
func TestRtreeI32ConstraintParityWithMirrorTable(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	setup := []string{
		"CREATE VIRTUAL TABLE t1 USING rtree_i32(ii, x1, x2, y1, y2)",
		"CREATE TABLE t2 (ii, x1 INT, x2 INT, y1 INT, y2 INT)",
	}
	for _, stmt := range setup {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("%s: %v", stmt, res.Error)
		}
	}
	// Deterministic rows (the test uses int(rand()*1000); these cover
	// equal, straddling and outside values for the probe constants).
	rows := [][5]int{
		{1, 0, 100, 0, 100},
		{2, 50, 150, 50, 150},
		{3, 100, 200, 100, 200},
		{4, 500, 600, 500, 600},
		{5, 750, 850, 250, 350},
		// rtree2.test's coordinates are int(rand()*1000) per column; inverted
		// pairs (x1 > x2) are rejected by BOTH SQLite and frigolite with
		// "rtree constraint failed: t1.(x1<=x2)" (oracle-verified), so the
		// test retries them — only degenerate (x1 == x2) rows reach the
		// tables alongside the normal ones.
		{7, 400, 400, 300, 950},
		// duplicate coordinates under different ids
		{8, 0, 100, 0, 100},
		{9, 100, 200, 100, 200},
	}
	for _, r := range rows {
		ins := fmt.Sprintf("INSERT INTO t1 VALUES(%d, %d, %d, %d, %d)", r[0], r[1], r[2], r[3], r[4])
		if res := db.Exec(ins); res.Error != nil {
			t.Fatalf("%s: %v", ins, res.Error)
		}
		ins2 := fmt.Sprintf("INSERT INTO t2 VALUES(%d, %d, %d, %d, %d)", r[0], r[1], r[2], r[3], r[4])
		if res := db.Exec(ins2); res.Error != nil {
			t.Fatalf("%s: %v", ins2, res.Error)
		}
	}

	ops := []string{"<", ">", "<=", ">=", "="}
	columns := []string{"ii", "x1", "x2", "y1", "y2"}
	values := []int{-1, 0, 50, 100, 499, 500, 750, 1001}
	checked := 0
	for _, col := range columns {
		for _, op := range ops {
			for _, val := range values {
				where := fmt.Sprintf("%s %s %d", col, op, val)
				q1 := db.Query("SELECT ii FROM t1 WHERE " + where + " ORDER BY ii")
				if q1.Error != nil {
					t.Fatalf("rtree query %q: %v", where, q1.Error)
				}
				q2 := db.Query("SELECT ii FROM t2 WHERE " + where + " ORDER BY ii")
				if q2.Error != nil {
					t.Fatalf("mirror query %q: %v", where, q2.Error)
				}
				checked++
				got1 := renderRows(q1.Rows)
				got2 := renderRows(q2.Rows)
				if got1 != got2 {
					t.Fatalf("predicate %q diverges: rtree returns [%s], mirror returns [%s]",
						where, got1, got2)
				}
			}
		}
	}
	// Multi-predicate mixes (rtree2 section 4 uses three predicates ANDed,
	// possibly on REPEATED columns).
	for _, col1 := range columns {
		for _, col2 := range columns {
			for _, col3 := range columns {
				where := fmt.Sprintf("%s >= 50 AND %s <= 600 AND %s > 200", col1, col2, col3)
				q1 := db.Query("SELECT ii FROM t1 WHERE " + where + " ORDER BY ii")
				q2 := db.Query("SELECT ii FROM t2 WHERE " + where + " ORDER BY ii")
				checked++
				got1, gerr1 := renderRows(q1.Rows), q1.Error
				got2, gerr2 := renderRows(q2.Rows), q2.Error
				if fmt.Sprint(gerr1) != fmt.Sprint(gerr2) || got1 != got2 {
					t.Fatalf("predicate %q diverges: rtree [%v/%s], mirror [%v/%s]",
						where, gerr1, got1, gerr2, got2)
				}
			}
		}
	}
	t.Logf("checked %d predicates, all agree", checked)
}

// rtree2.test section 5 (the 102 failing leaves, nDim=3): DELETE FROM the
// rtree with a WHERE clause must leave exactly the rows the mirror table
// keeps. Each iteration deletes ii <= N for growing N.
func TestRtreeI32DeleteWithWhereMatchesMirrorTable(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, stmt := range []string{
		"CREATE VIRTUAL TABLE t1 USING rtree_i32(ii, x1, x2, y1, y2, z1, z2)",
		"CREATE TABLE t2 (ii, x1 INT, x2 INT, y1 INT, y2 INT, z1 INT, z2 INT)",
	} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("%s: %v", stmt, res.Error)
		}
	}
	// 60 rows (the failures start around ii=780 of 1000 rows; a smaller set
	// exercises the same delete pattern).
	for ii := 0; ii < 60; ii++ {
		x1, x2 := ii*7%1000, ii*7%1000+ii%50
		y1, y2 := ii*13%1000, ii*13%1000+ii%80
		z1, z2 := ii*29%1000, ii*29%1000+ii%30
		ins := fmt.Sprintf("INSERT INTO t1 VALUES(%d, %d, %d, %d, %d, %d, %d)", ii, x1, x2, y1, y2, z1, z2)
		if res := db.Exec(ins); res.Error != nil {
			t.Fatalf("%s: %v", ins, res.Error)
		}
		ins2 := fmt.Sprintf("INSERT INTO t2 VALUES(%d, %d, %d, %d, %d, %d, %d)", ii, x1, x2, y1, y2, z1, z2)
		if res := db.Exec(ins2); res.Error != nil {
			t.Fatalf("%s: %v", ins2, res.Error)
		}
	}

	for _, n := range []int{0, 1, 5, 7, 13, 25, 40, 59, 60} {
		for _, tbl := range []string{"t2", "t1"} {
			del := fmt.Sprintf("DELETE FROM %s WHERE ii <= %d", tbl, n)
			if res := db.Exec(del); res.Error != nil {
				t.Fatalf("%s: %v", del, res.Error)
			}
		}
		q1 := db.Query("SELECT * FROM t1 ORDER BY ii")
		q2 := db.Query("SELECT * FROM t2 ORDER BY ii")
		if q1.Error != nil || q2.Error != nil {
			t.Fatalf("compare queries: %v / %v", q1.Error, q2.Error)
		}
		if renderRows(q1.Rows) != renderRows(q2.Rows) {
			t.Fatalf("after DELETE ii <= %d: rtree rows [%s] != mirror rows [%s]",
				n, renderRows(q1.Rows), renderRows(q2.Rows))
		}
	}
}

func renderRows(rows [][]interface{}) string {
	out := ""
	for _, row := range rows {
		for _, v := range row {
			out += fmt.Sprint(v) + " "
		}
	}
	return out
}
var rtree2I32Rows = [][7]int{
	{4, 113, 747, 609, 721, 190, 433},
	{8, 201, 909, 437, 709, 74, 785},
	{39, 108, 122, 756, 824, 150, 309},
	{50, 725, 755, 678, 685, 8, 142},
	{61, 10, 251, 198, 767, 17, 362},
	{80, 106, 401, 188, 363, 321, 753},
	{94, 776, 907, 109, 488, 18, 55},
	{96, 341, 451, 307, 860, 402, 727},
	{97, 383, 802, 896, 992, 56, 914},
	{103, 63, 859, 79, 393, 366, 994},
	{112, 638, 672, 153, 666, 59, 468},
	{120, 229, 927, 429, 964, 818, 905},
	{136, 37, 118, 416, 644, 113, 297},
	{139, 7, 541, 464, 700, 900, 921},
	{144, 778, 931, 47, 819, 620, 850},
	{147, 78, 254, 158, 166, 252, 266},
	{148, 443, 923, 531, 997, 185, 471},
	{160, 128, 420, 104, 646, 903, 945},
	{166, 39, 387, 566, 897, 764, 990},
	{168, 189, 550, 253, 455, 233, 807},
	{174, 1, 300, 442, 883, 507, 950},
	{175, 404, 718, 307, 852, 132, 258},
	{190, 118, 171, 727, 758, 20, 113},
	{210, 335, 640, 87, 776, 63, 524},
	{216, 591, 596, 825, 943, 431, 641},
	{219, 266, 928, 36, 128, 303, 728},
	{228, 302, 896, 55, 622, 32, 306},
	{231, 288, 377, 330, 774, 412, 623},
	{234, 249, 576, 526, 884, 208, 826},
	{241, 229, 948, 280, 976, 417, 788},
	{244, 711, 790, 295, 405, 379, 410},
	{245, 361, 678, 89, 281, 239, 832},
	{258, 359, 508, 72, 663, 498, 930},
	{275, 67, 628, 691, 801, 477, 544},
	{289, 102, 130, 422, 484, 71, 896},
	{297, 316, 530, 270, 715, 432, 810},
	{301, 175, 415, 41, 350, 370, 703},
	{311, 217, 605, 53, 765, 517, 887},
	{327, 212, 681, 123, 925, 53, 608},
	{330, 825, 840, 199, 979, 14, 140},
	{341, 756, 800, 415, 428, 278, 460},
	{348, 43, 585, 12, 619, 654, 981},
	{354, 99, 343, 515, 650, 276, 682},
	{355, 237, 787, 437, 837, 304, 808},
	{359, 709, 828, 108, 751, 84, 356},
	{365, 550, 907, 203, 908, 500, 591},
	{367, 309, 490, 327, 708, 367, 464},
	{369, 219, 531, 2, 537, 138, 627},
	{374, 491, 622, 217, 263, 584, 881},
	{389, 39, 814, 336, 693, 136, 630},
	{401, 224, 770, 235, 347, 651, 777},
	{433, 339, 850, 765, 821, 363, 672},
	{435, 505, 956, 541, 907, 149, 353},
	{451, 43, 509, 275, 423, 652, 657},
	{453, 374, 810, 120, 869, 588, 614},
	{468, 775, 854, 238, 251, 118, 726},
	{472, 226, 485, 502, 575, 646, 647},
	{477, 75, 162, 41, 288, 4, 279},
	{478, 641, 876, 32, 580, 592, 780},
	{483, 170, 218, 591, 648, 447, 629},
	{491, 174, 358, 233, 589, 37, 129},
	{528, 481, 765, 591, 943, 814, 961},
	{559, 529, 998, 106, 223, 166, 814},
	{566, 653, 718, 416, 985, 85, 553},
	{571, 239, 470, 447, 504, 280, 318},
	{591, 53, 76, 213, 813, 615, 991},
	{609, 331, 872, 67, 482, 337, 415},
	{614, 129, 827, 503, 997, 115, 330},
	{615, 794, 872, 28, 424, 68, 529},
	{618, 464, 722, 304, 794, 10, 163},
	{619, 98, 812, 858, 988, 612, 659},
	{621, 626, 926, 11, 708, 268, 575},
	{631, 155, 410, 139, 434, 202, 690},
	{632, 261, 997, 401, 418, 52, 161},
	{634, 156, 269, 234, 348, 40, 949},
	{642, 516, 907, 211, 706, 483, 856},
	{644, 470, 500, 42, 158, 9, 960},
	{647, 5, 332, 927, 930, 284, 381},
	{652, 744, 852, 38, 489, 53, 989},
	{658, 498, 837, 662, 803, 34, 791},
	{660, 51, 169, 601, 735, 30, 249},
	{666, 859, 949, 479, 573, 61, 973},
	{674, 183, 334, 136, 242, 68, 887},
	{705, 82, 753, 276, 995, 33, 620},
	{734, 311, 932, 188, 382, 549, 733},
	{737, 0, 405, 223, 631, 131, 557},
	{766, 752, 828, 376, 511, 221, 726},
	{775, 254, 470, 324, 415, 17, 452},
	{820, 671, 713, 450, 813, 117, 224},
	{839, 6, 405, 95, 371, 509, 635},
	{840, 803, 909, 264, 514, 372, 749},
	{841, 206, 607, 69, 661, 617, 711},
	{851, 486, 883, 307, 559, 139, 966},
	{852, 889, 970, 749, 859, 732, 831},
	{856, 426, 673, 350, 983, 535, 614},
	{863, 654, 868, 559, 873, 0, 761},
	{864, 263, 421, 625, 939, 575, 920},
	{869, 46, 421, 414, 867, 830, 983},
	{872, 22, 984, 756, 758, 35, 990},
	{885, 527, 815, 97, 378, 286, 401},
	{891, 455, 550, 604, 977, 413, 424},
	{909, 540, 650, 85, 707, 244, 884},
	{911, 383, 967, 160, 819, 256, 314},
	{914, 710, 925, 66, 406, 671, 817},
	{916, 25, 124, 737, 905, 202, 577},
	{921, 26, 590, 175, 328, 308, 882},
	{922, 106, 382, 440, 477, 158, 561},
	{932, 417, 566, 540, 853, 109, 985},
	{937, 219, 963, 272, 918, 473, 656},
	{938, 951, 953, 585, 770, 806, 884},
	{946, 67, 560, 168, 896, 13, 964},
	{949, 324, 971, 110, 182, 234, 514},
	{959, 463, 705, 151, 482, 13, 385},
	{971, 89, 470, 138, 956, 379, 994},
	{972, 121, 934, 339, 465, 754, 772},
	{974, 45, 838, 513, 810, 24, 816},
	{990, 612, 629, 159, 981, 541, 977},
	{994, 838, 967, 532, 544, 91, 895},
	{997, 105, 389, 184, 364, 674, 945},
}

// TestRtreeI32ExactFixtureRowsDeleteParity replays the EXACT row set the
// rtree2 fixture installs (extracted from testdata/rtree2.json's 3.2 case)
// and the section-5 delete ladder, where the 102 run-31 failures live.
func TestRtreeI32ExactFixtureRowsDeleteParity(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		"CREATE VIRTUAL TABLE t1 USING rtree_i32(ii, x1, x2, y1, y2, z1, z2)",
		"CREATE TABLE t2 (ii, x1 INT, x2 INT, y1 INT, y2 INT, z1 INT, z2 INT)",
	} {
		if res := db.Exec(stmt); res.Error != nil {
			t.Fatalf("%s: %v", stmt, res.Error)
		}
	}
	for _, r := range rtree2I32Rows {
		ins := fmt.Sprintf("INSERT INTO t1 VALUES(%d, %d, %d, %d, %d, %d, %d)", r[0], r[1], r[2], r[3], r[4], r[5], r[6])
		if res := db.Exec(ins); res.Error != nil {
			t.Fatalf("%s: %v", ins, res.Error)
		}
		ins2 := fmt.Sprintf("INSERT INTO t2 VALUES(%d, %d, %d, %d, %d, %d, %d)", r[0], r[1], r[2], r[3], r[4], r[5], r[6])
		if res := db.Exec(ins2); res.Error != nil {
			t.Fatalf("%s: %v", ins2, res.Error)
		}
	}
	for n := 0; n < 1000; n += 10 {
		for _, tbl := range []string{"t2", "t1"} {
			del := fmt.Sprintf("DELETE FROM %s WHERE ii <= %d", tbl, n)
			if res := db.Exec(del); res.Error != nil {
				t.Fatalf("%s: %v", del, res.Error)
			}
		}
		q1 := db.Query("SELECT * FROM t1 ORDER BY ii")
		q2 := db.Query("SELECT * FROM t2 ORDER BY ii")
		if renderRows(q1.Rows) != renderRows(q2.Rows) {
			t.Fatalf("after DELETE ii <= %d: rtree rows [%s] != mirror rows [%s]",
				n, renderRows(q1.Rows), renderRows(q2.Rows))
		}
	}
}
