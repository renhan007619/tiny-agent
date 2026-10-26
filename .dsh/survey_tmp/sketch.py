# -*- coding: utf-8 -*-
"""生成闭合水准路线草图 PNG"""
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt

plt.rcParams["font.sans-serif"] = ["Microsoft YaHei", "SimHei"]
plt.rcParams["axes.unicode_minus"] = False

CIRCLED = "①②③④⑤⑥⑦⑧⑨⑩"

def make(path, nodes):
    fig, ax = plt.subplots(figsize=(4.4, 3.4), dpi=220)
    n = len(nodes)
    for i in range(n):
        (x1, y1) = nodes[i][1], nodes[i][2]
        (x2, y2) = nodes[(i + 1) % n][1], nodes[(i + 1) % n][2]
        ax.annotate("", xy=(x2, y2), xytext=(x1, y1),
                    arrowprops=dict(arrowstyle="-|>", color="black", lw=1.4,
                                    shrinkA=10, shrinkB=10, mutation_scale=14))
        mx, my = (x1 + x2) / 2, (y1 + y2) / 2
        dx, dy = x2 - x1, y2 - y1
        L = (dx**2 + dy**2) ** 0.5
        ox, oy = -dy / L * 0.22, dx / L * 0.22
        ax.text(mx + ox, my + oy, CIRCLED[i], fontsize=13, ha="center", va="center")
    for name, x, y, sub in nodes:
        ax.plot([x], [y], "o", color="black", ms=5)
        if name == "BM.A":
            ax.text(x, y + 0.16, name, fontsize=11, ha="center", fontweight="bold")
            if sub:
                ax.text(x, y - 0.30, sub, fontsize=9, ha="center")
        else:
            ax.text(x, y + 0.16, name, fontsize=11, ha="center")
    ax.set_aspect("equal")
    ax.axis("off")
    pad = 0.55
    xs = [nd[1] for nd in nodes]
    ys = [nd[2] for nd in nodes]
    ax.set_xlim(min(xs) - pad, max(xs) + pad)
    ax.set_ylim(min(ys) - pad - 0.25, max(ys) + pad)
    fig.tight_layout(pad=0.3)
    fig.savefig(path, bbox_inches="tight", facecolor="white")
    plt.close(fig)

if __name__ == "__main__":
    make("sketch1.png", nodes=[("BM.A", 0.5, 1.9), ("P1", 2.3, 1.5), ("P2", 2.7, 0.3),
                               ("P3", 1.3, -0.5), ("P4", -0.3, 0.2)])
    make("sketch4.png", nodes=[("BM.A", -0.3, 1.7), ("P1", 1.2, 2.0), ("P2", 2.6, 1.0),
                               ("P3", 2.2, -0.5), ("P4", 0.5, -1.0), ("P5", -1.0, -0.3)])
    print("sketches done")
