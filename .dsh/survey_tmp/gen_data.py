# -*- coding: utf-8 -*-
"""工程测量C 课间实验报告 - 数据生成 + 验证 + 填表"""
import math

def r3(x):  # 保留3位小数，四舍五入(远离零)
    return math.copysign(math.floor(abs(x) * 1000 + 0.5), x) / 1000

def dms(d, m, s):
    return f"{d}°{m:02d}′{s:02d}″"

def sec(d, m, s):
    return d * 3600 + m * 60 + s

def vsec(d, m, s):
    """d 为带符号度数，m/s 恒为正：-2,35,18 表示 -2°35′18″"""
    return -(abs(d) * 3600 + m * 60 + s) if d < 0 else d * 3600 + m * 60 + s

def vdms(d, m, s):
    return f"-{abs(d)}°{m:02d}′{s:02d}″" if d < 0 else f"{d}°{m:02d}′{s:02d}″"

def from_sec(t):
    t = round(t)
    d = t // 3600; t %= 3600
    m = t // 60; s = t % 60
    return d, m, s

def fmt_sec(t):
    t = round(t)
    sign = "-" if t < 0 else ""
    t = abs(t)
    d = t // 3600
    t %= 3600
    m = t // 60
    s = t % 60
    return f"{sign}{d}°{m:02d}′{s:02d}″"

# =====================================================================
# 实验一：闭合水准路线（普通水准）  5站  BM.A-P1-P2-P3-P4-BM.A
# =====================================================================
H_A1 = 156.328
exp1 = [  # (后视点, 前视点, a, b)
    ("BM.A", "P1", 1.647, 1.329),
    ("P1", "P2", 1.283, 1.428),
    ("P2", "P3", 0.984, 1.386),
    ("P3", "P4", 1.516, 1.229),
    ("P4", "BM.A", 1.371, 1.421),
]
exp1_h = [round(a - b, 3) for _, _, a, b in exp1]
sum_a = round(sum(x[2] for x in exp1), 3)
sum_b = round(sum(x[3] for x in exp1), 3)
sum_h = round(sum(exp1_h), 3)
fh1_mm = round(sum_h * 1000)
tol1_mm = 12 * math.sqrt(5)
# 观测高程（未平差，逐步推算），按站记录(后视点高程, 前视点高程)
exp1_Hobs = []
H = H_A1
for (bk, fr, a, b), h in zip(exp1, exp1_h):
    exp1_Hobs.append((H, round(H + h, 3)))
    H = round(H + h, 3)
assert H == round(H_A1 + sum_h, 3)
sum_pos = round(sum(h for h in exp1_h if h > 0), 3)
sum_neg = round(sum(-h for h in exp1_h if h < 0), 3)

assert abs((sum_a - sum_b) - sum_h) < 1e-9, "exp1 检核失败"
assert abs(fh1_mm) <= tol1_mm, "exp1 闭合差超限"

# =====================================================================
# 实验二：全站仪  测点O，目标A/B，3名组员轮流观测∠AOB
# =====================================================================
exp2 = [  # (A水平度盘读数, A竖直角, A斜距, B读数, B竖直角, B斜距, 备注)
    ((0, 0, 4), (4, 12, 30), 46.352, (68, 24, 40), (-2, 35, 18), 58.713, "仪器高1.482m，棱镜高1.500m"),
    ((0, 0, 18), (4, 12, 24), 46.350, (68, 24, 46), (-2, 35, 24), 58.716, ""),
    ((0, 0, 10), (4, 12, 36), 46.353, (68, 24, 52), (-2, 35, 12), 58.711, ""),
]
exp2_calc = []  # 每单元: (beta秒, A(HR,VA,SD,HD,VD), B(...), note)
betas = []
for (ra, va, sda, rb, vb, sdb, note) in exp2:
    beta = sec(*rb) - sec(*ra)
    betas.append(beta)
    rows = []
    for hr, v, sd in ((ra, va, sda), (rb, vb, sdb)):
        rad = math.radians(vsec(*v) / 3600.0)
        rows.append(dict(hr=hr, va=v, sd=sd, hd=r3(sd * math.cos(rad)), vd=r3(sd * math.sin(rad))))
        assert abs(math.hypot(rows[-1]["hd"], rows[-1]["vd"]) - sd) < 0.003, "exp2 SD/HD/VD 不自洽"
    exp2_calc.append((beta, rows, note))
assert max(betas) - min(betas) <= 40, "exp2 各测回水平角互差超限"

# =====================================================================
# 实验三：测回法  测站O，目标A、B，水平角2测回 + 竖直角2目标x2测回
# =====================================================================
# 水平角：(测回, 盘位, 目标, 读数)
exp3h = [
    (1, "左", "A", (0, 1, 18)), (1, "左", "B", (73, 46, 30)),
    (1, "右", "B", (253, 46, 42)), (1, "右", "A", (180, 1, 36)),
    (2, "左", "A", (90, 1, 24)), (2, "左", "B", (163, 46, 48)),
    (2, "右", "B", (343, 46, 54)), (2, "右", "A", (270, 1, 42)),
]
half_beta = {}   # (测回, 盘位) -> 半测回角值(秒)
for setn in (1, 2):
    bl = sec(*exp3h[setn * 4 - 4 + 1][3]) - sec(*exp3h[setn * 4 - 4][3])         # 盘左 B-A
    br = sec(*exp3h[setn * 4 - 4 + 2][3]) - sec(*exp3h[setn * 4 - 4 + 3][3])     # 盘右 B-A
    half_beta[(setn, "左")] = bl
    half_beta[(setn, "右")] = br
    d_beta = abs(bl - br)
    assert d_beta <= 40, f"exp3 测回{setn} 上下半测回互差 {d_beta}″ 超限"
beta1 = (half_beta[(1, "左")] + half_beta[(1, "右")]) / 2
beta2 = (half_beta[(2, "左")] + half_beta[(2, "右")]) / 2
assert abs(beta1 - beta2) <= 40, "exp3 测回间互差超限"
beta_avg = (beta1 + beta2) / 2

# 竖直角：(目标, 测回, 盘位, 竖盘读数)   α左=90°-L, α右=R-270°, x=(L+R-360)/2
exp3v = [
    ("A", 1, "左", (83, 24, 36)), ("A", 1, "右", (276, 35, 48)),
    ("A", 2, "左", (83, 24, 30)), ("A", 2, "右", (276, 36, 6)),
    ("B", 1, "左", (93, 13, 0)), ("B", 1, "右", (266, 47, 24)),
    ("B", 2, "左", (93, 12, 54)), ("B", 2, "右", (266, 47, 42)),
]
v_res = {}  # (目标, 测回) -> (α左, α右, x, α一测回)
for tgt in ("A", "B"):
    for setn in (1, 2):
        L = sec(*[r for t, s, p, r in exp3v if t == tgt and s == setn and p == "左"][0:1][0]) if False else None
        L = sec(*[r for t, s, p, r in exp3v if t == tgt and s == setn and p == "左"][0])
        R = sec(*[r for t, s, p, r in exp3v if t == tgt and s == setn and p == "右"][0])
        al = sec(90, 0, 0) - L
        ar = R - sec(270, 0, 0)
        x = (L + R - sec(360, 0, 0)) / 2
        v_res[(tgt, setn)] = (al, ar, x, (al + ar) / 2)
        assert abs(x) <= 25, "exp3 指标差超限"
v_avg = {"A": (v_res[("A", 1)][3] + v_res[("A", 2)][3]) / 2,
         "B": (v_res[("B", 1)][3] + v_res[("B", 2)][3]) / 2}

# =====================================================================
# 实验四：四等水准  6站  BM.A-P1-P2-P3-P4-P5-BM.A
# =====================================================================
H_A4 = 162.474
K1, K2 = 4.687, 4.787
# 每站: (起点, 终点, 后尺号, 前尺号, 后下丝, 后上丝, 后中丝黑, 前下丝, 前上丝, 前中丝黑, εa, εb)
# ε = 黑+K-红 的显示值(mm)；a红 = a黑+K-ε
exp4 = [
    ("BM.A", "P1", 1, 2, 1.777, 1.271, 1.524, 1.421, 0.923, 1.172, -1, +1),
    ("P1", "P2", 2, 1, 1.624, 1.151, 1.387, 1.749, 1.260, 1.505, -1, -1),
    ("P2", "P3", 1, 2, 1.726, 1.195, 1.461, 1.459, 0.935, 1.197, +1, +1),
    ("P3", "P4", 2, 1, 1.541, 1.055, 1.298, 1.938, 1.469, 1.704, -2, 0),
    ("P4", "P5", 1, 2, 1.632, 1.080, 1.356, 1.419, 0.871, 1.145, -1, +1),
    ("P5", "BM.A", 2, 1, 1.654, 1.159, 1.407, 1.957, 1.450, 1.703, 0, -2),
]
st4 = []
sum_d_cum = 0.0
for (bk, fr, no_b, no_f, bl, bu, a, fl, fu, b, ea, eb) in exp4:
    Kb, Kf = (K1, K2) if no_b == 1 else (K2, K1)
    Db = round((bl - bu) * 100, 1)
    Df = round((fl - fu) * 100, 1)
    d = round(Db - Df, 1)
    sum_d_cum = round(sum_d_cum + d, 1)
    a_red = round(a + Kb - ea / 1000, 3)
    b_red = round(b + Kf - eb / 1000, 3)
    h_b = round(a - b, 3)
    h_r = round(a_red - b_red, 3)
    ediff = ea - eb
    h_mid = round(h_b - ediff / 2000, 4)
    # 检核
    assert abs(round((a + Kb) - a_red, 3) * 1000 - ea) < 1e-6
    assert abs(round((b + Kf) - b_red, 3) * 1000 - eb) < 1e-6
    assert abs(ea) <= 3 and abs(eb) <= 3 and abs(ediff) <= 5, "exp4 黑红面读数差超限"
    assert abs(d) <= 3 and abs(sum_d_cum) <= 10, "exp4 视距差超限"
    assert abs((h_r - h_b) * 1000 - (Kb - Kf) * 1000 + ediff) < 1e-6, "exp4 红面高差不自洽"
    assert abs(round((bl + bu) / 2 - a, 3)) <= 0.003 and abs(round((fl + fu) / 2 - b, 3)) <= 0.003, "exp4 中丝与视距丝不自洽"
    st4.append(dict(bk=bk, fr=fr, no_b=no_b, no_f=no_f, bl=bl, bu=bu, a=a, fl=fl, fu=fu,
                    b=b, ea=ea, eb=eb, a_red=a_red, b_red=b_red, Db=Db, Df=Df, d=d,
                    cum=sum_d_cum, h_b=h_b, h_r=h_r, ediff=ediff, h_mid=h_mid))
sum_hb = round(sum(s["h_b"] for s in st4), 3)
sum_hr = round(sum(s["h_r"] for s in st4), 3)
sum_hm = round(sum(s["h_mid"] for s in st4), 4)
assert abs((sum_hb + sum_hr) / 2 - sum_hm) < 1e-9, "exp4 Σh中 检核失败"
fh4_mm = round(sum_hm * 1000)
L4 = round(sum(s["Db"] + s["Df"] for s in st4), 1)
tol4_mm = 20 * math.sqrt(L4 / 1000)
assert abs(fh4_mm) <= tol4_mm, "exp4 闭合差超限"
# 成果计算：改正数按距离比例分配，总和=-fh
raw = [-fh4_mm * (s["Db"] + s["Df"]) / L4 for s in st4]
v4 = [round(x) for x in raw]
i = max(range(6), key=lambda k: abs(raw[k] - round(raw[k])) + (0.001 if v4[k] > raw[k] else 0))
while sum(v4) != -fh4_mm:
    v4[i] += -1 if sum(v4) > -fh4_mm else 1
    i = (i + 1) % 6
assert sum(v4) == -fh4_mm
H4 = {"BM.A": H_A4}
Hc = H_A4
for s, v in zip(st4, v4):
    Hc = round(Hc + s["h_mid"] + v / 1000, 3)
    H4[s["fr"]] = Hc
assert H4["BM.A"] == H_A4, "exp4 平差后不闭合"
seg_len = [round(s["Db"] + s["Df"], 1) for s in st4]

# ------------------ 打印验证报告 ------------------
print("=" * 60)
print("实验一  普通闭合水准  Σa=%.3f Σb=%.3f Σh=%+.3f  fh=%+dmm  容许=±12√5=±%.1fmm" %
      (sum_a, sum_b, sum_h, fh1_mm, tol1_mm))
for (bk, fr, a, b), h in zip(exp1, exp1_h):
    print("   %s→%s  a=%.3f b=%.3f  h=%+.3f  H后=%.3f H前=%.3f" % (bk, fr, a, b, h, *exp1_Hobs[[x[0] for x in exp1].index(bk)]))
print("   Σ正=%.3f Σ负=%.3f" % (sum_pos, sum_neg))
print("=" * 60)
print("实验二  全站仪")
for i, (beta, rows, note) in enumerate(exp2_calc, 1):
    a, b = rows
    print("   第%d组 ∠AOB=%s | A: HR=%s V=%s SD=%.3f HD=%.3f VD=%+.3f | B: HR=%s V=%s SD=%.3f HD=%.3f VD=%+.3f" %
          (i, fmt_sec(beta), dms(*a["hr"]), vdms(*a["va"]), a["sd"], a["hd"], a["vd"],
           dms(*b["hr"]), vdms(*b["va"]), b["sd"], b["hd"], b["vd"]))
    if note:
        print("      备注:", note)
print("=" * 60)
print("实验三  测回法水平角")
for setn in (1, 2):
    print("   测回%d: β左=%s β右=%s 互差=%d″  一测回=%s" %
          (setn, fmt_sec(half_beta[(setn, '左')]), fmt_sec(half_beta[(setn, '右')]),
           abs(half_beta[(setn, '左')] - half_beta[(setn, '右')]), fmt_sec(beta1 if setn == 1 else beta2)))
print("   各测回平均=%s" % fmt_sec(beta_avg))
print("   竖直角:")
for tgt in ("A", "B"):
    for setn in (1, 2):
        al, ar, x, am = v_res[(tgt, setn)]
        print("   目标%s 测回%d: α左=%s α右=%s x=%+.0f″ 一测回=%s" % (tgt, setn, fmt_sec(al), fmt_sec(ar), x, fmt_sec(am)))
    print("   目标%s 平均=%s" % (tgt, fmt_sec(v_avg[tgt])))
print("=" * 60)
print("实验四  四等水准")
for i, s in enumerate(st4, 1):
    print("   站%d %s→%s: 后尺%d D后=%.1f 前尺%d D前=%.1f d=%+.1f Σd=%+.1f | εa=%+d εb=%+d ε差=%+d | h黑=%+.3f h红=%+.3f h中=%+.4f" %
          (i, s["bk"], s["fr"], s["no_b"], s["Db"], s["no_f"], s["Df"], s["d"], s["cum"],
           s["ea"], s["eb"], s["ediff"], s["h_b"], s["h_r"], s["h_mid"]))
print("   Σh黑=%+.3f Σh红=%+.3f Σh中=%+.4f  fh=%+dmm  L=%.1fm  容许=±20√%.3f=±%.1fmm" %
      (sum_hb, sum_hr, sum_hm, fh4_mm, L4, L4 / 1000, tol4_mm))
print("   成果: 改正数(mm)=%s  改正后Σ=%.3f" % (v4, sum(s["h_mid"] + v / 1000 for s, v in zip(st4, v4))))
for s, v in zip(st4, v4):
    print("     %s: L=%.1f h测=%+.4f v=%+dmm h改=%+.4f H=%.3f" % (s["fr"], s["Db"] + s["Df"], s["h_mid"], v, s["h_mid"] + v / 1000, H4[s["fr"]]))
print("ALL CHECKS PASSED")
