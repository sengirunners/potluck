"""mezze.sh's live robot: the low-poly robot of Mezze's picture (Mezze's private design source,
assets/social-preview/mezze-social-preview-scene.py: same shapes, colours and pose, fewer segments for real time),
split into the parts the page animates and written as one small binary, mezze/public/3d/robot.bin, that
mezze/public/robot.js draws with WebGL. Run by hand after changing the model, and commit what it writes:

    blender -b --python build/mezze-robot.py          (Blender 5.2; a few seconds; no render)

The file: "MZR1", a uint32 header length, the header (JSON, padded to 4 bytes), then per part, in header order:
positions (int16 x3, quantised to the header's centre and scale), normals (int8 x3), colours (uint8 x4: sRGB, and
the material's roughness x200, or 255 for a glowing material), each array padded to 4 bytes, and triangle indices
(uint16 x3). Positions are the picture's pose in Blender's world space (z up, -y towards the camera); each part names
its parent and the pivot it turns about, and each plate its centre, its axis and the top of its stick.
"""
import bpy, bmesh, math, json, os, random, struct, sys
from mathutils import Vector, Matrix

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.join(HERE, "..", "mezze", "public", "3d", "robot.bin")
RW, RH = 1100, 1200  # the picture's render size: the canvas keeps its aspect, so the fallback image lines up
random.seed(4)
bpy.ops.wm.read_factory_settings(use_empty=True)
sc = bpy.context.scene
sys.dont_write_bytecode = True
sys.path.insert(0, HERE)
from mezze_shapes import *  # the helpers, the palette, the plate and the tapas (build/mezze_shapes.py)

# Parts: every object made while a part is open belongs to it.
PARTS = []  # [name, parent, pivot, set of objects, extra]
def part(name, parent, pivot, extra=None):
    PARTS.append([name, parent, Vector(pivot) if pivot is not None else None, set(bpy.data.objects), extra or {}])
def close_parts():
    seen = set()
    for i, p in enumerate(PARTS):
        after = set(PARTS[i + 1][3]) if i + 1 < len(PARTS) else set(bpy.data.objects)
        # Sorted by name, so the file is the same byte for byte on every run.
        p[3] = sorted((o for o in after if o not in p[3] and o not in seen and o.type == "MESH"), key=lambda o: o.name)
        seen.update(p[3])

# ---------- robot (the picture's construction, in parts) ----------
LEAN, HEADROLL = 12, 7
PIV = Vector((0.0, 0.0, 1.35))
def grp(before, pivot, rot_y):
    new = [o for o in bpy.data.objects if o not in before and o.parent is None]
    E = bpy.data.objects.new("piv", None); sc.collection.objects.link(E); E.location = pivot
    for o in new:
        o.parent = E; o.matrix_parent_inverse = Matrix.Translation(-Vector(pivot))
    E.rotation_euler = (0, math.radians(rot_y), 0)
    return E
def empty(name, loc):
    e = bpy.data.objects.new(name, None); sc.collection.objects.link(e); e.location = loc; return e

part("root", None, (0.06, -0.1, 0.07))
limb((0.0, 0, 0.3), (0.0, 0, 1.35), 0.16, sage)
sphere((0.0, 0, 0.5), 0.2, cream, 1)
box((0.06, -0.1, 0.14), (0.44, 0.58, 0.14), terra, bevel=0.07)
part("kick", "root", (-0.02, 0, 1.32))
sphere((-0.02, 0, 1.32), 0.3, cream, 1)
limb((-0.05, 0, 1.3), (-0.9, 0.1, 0.8), 0.15, sage)
sphere((-0.9, 0.1, 0.8), 0.19, cream, 1)
limb((-0.9, 0.1, 0.8), (-1.5, 0.1, 1.4), 0.14, sage)
sphere((-1.5, 0.1, 1.4), 0.17, cream, 1)
box((-1.62, 0.1, 1.5), (0.4, 0.22, 0.09), terra, rot=(0, math.radians(-38), 0), bevel=0.05)
STICK_FOOT = (-1.68, 0.1, 1.63)

_b = set(bpy.data.objects)
part("body", "root", PIV)
sphere((0, 0, 1.95), 1.0, cream, 2, (0.78, 0.6, 0.78))
sphere((0, -0.37, 1.9), 0.5, teal, 2, (0.9, 0.33, 0.95))
sphere((0, -0.58, 1.97), 0.12, mat("btn", RUBY, .15, emit=1.2), 1)
for i, c in enumerate((SAFF, AMET)):
    sphere((-0.15 + 0.3 * i, -0.56, 1.68), 0.075, mat("d" + c, c, .15, emit=.5), 1)
bpy.ops.mesh.primitive_torus_add(major_radius=0.7, minor_radius=0.075, major_segments=14, minor_segments=5, location=(0, 0, 1.45))
t = bpy.context.object; t.scale = (1, 0.78, 1); put(t, terra, True)
box((0, 0.5, 2.15), (0.4, 0.14, 0.3), sky, bevel=0.05)
for dx in (-0.25, 0, 0.25):
    cyl((dx, 0.5, 2.5), 0.065, 0.2, mat("saff", SAFF, .12, spec=1.0), 8)
RK = [empty("rk%d" % i, (dx, 0.5, 2.55)) for i, dx in enumerate((-0.25, 0, 0.25))]
SH_L, SH_R = (-0.72, 0, 2.3), (0.72, 0, 2.3)
EL_L, EL_R = (-1.25, -0.15, 2.55), (1.3, -0.2, 2.65)
HAND_L, HAND_R = (-1.75, -0.15, 3.05), (1.85, -0.1, 3.15)
for side, (a, b, c) in (("arm_l", (SH_L, EL_L, HAND_L)), ("arm_r", (SH_R, EL_R, HAND_R))):
    part(side, "body", a)  # pivot in the picture's pose is fixed below, once the lean is applied
    sphere(a, 0.2, sky, 1)
    limb(a, b, 0.12, sage); sphere(b, 0.15, cream, 1); limb(b, c, 0.12, sage)
    sphere(c, 0.19, terra, 1)
HL, HR = empty("hl", HAND_L), empty("hr", HAND_R)
SHL, SHR = empty("shl", SH_L), empty("shr", SH_R)
_h = set(bpy.data.objects)
HEAD = (0.0, -0.02, 3.2)
part("head", "body", (0, 0, 2.75))
sphere(HEAD, 1.0, cream, 2, (1.05, 0.85, 0.85))
box((0, -0.8, 3.18), (0.84, 0.08, 0.52), ink, bevel=0.14)
sphere((-0.3, -0.9, 3.25), 0.15, eye, 2, (1, 0.45, 1))
sphere((0.3, -0.9, 3.25), 0.15, eye, 2, (1, 0.45, 1))
limb((-0.12, -0.9, 2.94), (0.2, -0.9, 2.94), 0.05, eye, 6)
for sx in (-1, 1):
    cyl((sx * 1.02, 0, 3.15), 0.26, 0.2, terra, 10, rot=(0, math.radians(90), 0))
limb((0, 0, 3.95), (0.06, 0, 4.35), 0.045, stick_m, 6)
sphere((0.07, 0, 4.45), 0.15, red, 2)
NECK = empty("neck", (0, 0, 2.75))
EH = grp(_h, Vector((0, 0, 2.75)), HEADROLL)
EB = grp(_b, PIV, LEAN)
bpy.context.view_layer.update()
def W(o): return o.matrix_world.translation.copy()
hl, hr = W(HL), W(HR)
rk = [W(r) for r in RK]
for p in PARTS:  # the pivots, where they are in the picture's pose
    if p[0] == "arm_l": p[2] = W(SHL)
    if p[0] == "arm_r": p[2] = W(SHR)
    if p[0] == "head": p[2] = W(NECK)

# ---------- plates ----------
# name, plate centre (x, z), tilt, rim colour, the stick's start, the part that carries it, food
PL = [
    ("Ana's agents", (-1.68, 3.05), (26, 8), SAPH, STICK_FOOT, "kick", "bravas"),
    ("Leo's agents", (-1.26, 4.49), (24, -8), EMER, hl, "arm_l", "toast"),
    ("sessions", (-0.06, 5.46), (24, 4), SAFF, rk[0], "body", "grapes"),
    ("work items", (1.3, 4.43), (24, -6), AMET, rk[1], "body", "cheese"),
    ("events", (2.33, 5.4), (24, 8), TEAL, rk[2], "body", "prawns"),
    ("mail", (2.5, 3.9), (26, 10), RUBY, hr, "arm_r", "olives"),
]
knob = mat("knob", "#C48A2C", .4)
plates = []
for name, (px, pz), (tx, ty), col, start, carrier, food in PL:
    top = Vector((px, -0.05, pz - 0.05))
    part("stick:" + name, carrier, start)
    limb(start, top, 0.034, stick_m, 6)
    sphere(top, 0.055, knob, 1)
    part("plate:" + name, "stick:" + name, top, {"label": name})
    pl = plate_mesh(col)
    pl.scale = (1.08, 1.08, 1.08)
    pl.location = (px, -0.05, pz)
    pl.rotation_mode = 'ZYX'
    pl.rotation_euler = (math.radians(tx), math.radians(ty), math.radians(random.uniform(0, 360)))
    FOODS[food](pl)
    plates.append((name, pl))

# ---------- ground: the cazuela of olives and the striped napkin ----------
part("ground", None, None)
terracotta = mat("cazuela", "#D8582E", .35, spec=.6)
def bowl(loc, R, H):
    prof = [(0.0, 0.02), (R * .8, 0.02), (R, H), (R * 1.04, H + .02), (R * .94, H + .02), (R * .78, H * .5), (0.0, H * .45)]
    bm = bmesh.new(); vs = [bm.verts.new((r, 0, z)) for r, z in prof]
    es = [bm.edges.new((vs[i], vs[i + 1])) for i in range(len(vs) - 1)]
    bmesh.ops.spin(bm, geom=vs + es, cent=(0, 0, 0), axis=(0, 0, 1), angle=math.tau, steps=24)
    bmesh.ops.remove_doubles(bm, verts=bm.verts, dist=1e-4); bmesh.ops.recalc_face_normals(bm, faces=bm.faces)
    me = bpy.data.meshes.new("bowl"); bm.to_mesh(me); bm.free()
    o = bpy.data.objects.new("bowl", me); sc.collection.objects.link(o); o.location = loc
    me.materials.append(terracotta)
    for p in me.polygons: p.use_smooth = True
    return o
cz = bowl((1.7, -0.5, 0), 0.62, 0.2)
cyl((0, 0, 0.12), 0.5, 0.03, mat("oil", "#E8541E", .1, spec=1.0), 24, False, parent=cz)
for (x, y) in ((-0.2, 0.1), (0.15, 0.2), (0.2, -0.12), (-0.1, -0.2), (0.0, 0.0)):
    uvsphere((x, y, 0.18), 0.1, olive_g, (1.2, .9, .9), cz, 18)
    sphere((x + .08, y, 0.2), 0.04, red, 1, parent=cz)
nap_r, nap_c = mat("napr", RUBY, .6), mat("napc", "#FFF6E3", .7)
NP = empty("nap", (-1.4, -0.5, 0.02)); NP.rotation_euler = (0, 0, math.radians(18))
for k in range(11):
    b = box((0, -0.5 + 0.1 * k, 0.0), (0.62, 0.05, 0.012), nap_r if k % 2 == 0 else nap_c, bevel=0.004)
    b.parent = NP
fold = box((0.18, 0.15, 0.04), (0.3, 0.3, 0.012), nap_r, rot=(0, 0, math.radians(-20)), bevel=0.004); fold.parent = NP
for k in range(5):
    b = box((0.18, 0.15 - 0.24 + 0.1 * k, 0.056), (0.28, 0.05, 0.006), nap_c if k % 2 == 0 else nap_r); b.parent = NP; b.rotation_euler = (0, 0, math.radians(-20))
close_parts()

# ---------- camera and lights, as the picture has them ----------
th = math.radians(72); look = Vector((0, math.sin(th), -math.cos(th)))
target = Vector((0.1, 0, 2.95))
cam_m = Matrix.Translation(target - 20 * look) @ Matrix.Rotation(th, 4, "X")
view = cam_m.inverted()
sun_dir = (Matrix.Rotation(math.radians(-25), 3, "Z") @ Matrix.Rotation(math.radians(8), 3, "Y") @ Matrix.Rotation(math.radians(68), 3, "X")) @ Vector((0, 0, 1))
fill_dir = Vector((5, -8, 4)).normalized()

# ---------- write ----------
bpy.context.view_layer.update()
dg = bpy.context.evaluated_depsgraph_get()
def mesh_data(objs):
    verts, index, tris = [], {}, []
    for corners in triangles(objs, dg):
        tri = []
        for p, n, rgb, rough in corners:
            key = (round(p.x, 4), round(p.y, 4), round(p.z, 4), round(n.x * 127), round(n.y * 127), round(n.z * 127), rgb, rough)
            if key not in index:
                # Stored as rounded, as keyed: Blender's evaluation can differ in the last float digits between
                # runs, and the file must not.
                index[key] = len(verts); verts.append((Vector(key[:3]), Vector(key[3:6]) / 127, rgb, rough))
            tri.append(index[key])
        tris.append(tri)
    # Canonical order: vertices by value, triangles by their vertices (each turned to start at its lowest, keeping its
    # winding), so the file is the same byte for byte however Blender happened to order its evaluated meshes.
    keys = list(index.keys())
    order = sorted(range(len(verts)), key=lambda i: keys[i])
    new = {old: k for k, old in enumerate(order)}
    verts = [verts[i] for i in order]
    def turned(t):
        t = [new[i] for i in t]; m = t.index(min(t)); return tuple(t[m:] + t[:m])
    tris = sorted(turned(t) for t in tris)
    return verts, tris

data = [(p, mesh_data(p[3])) for p in PARTS]
allp = [v[0] for _, (vs, _) in data for v in vs]
lo = Vector((min(p.x for p in allp), min(p.y for p in allp), min(p.z for p in allp)))
hi = Vector((max(p.x for p in allp), max(p.y for p in allp), max(p.z for p in allp)))
centre = (lo + hi) / 2; scale = max(hi - lo) / 2 * 1.0001

def pad4(b): return b + b"\0" * (-len(b) % 4)
blobs, parts_json = [], []
for (name, parent, pivot, objs, extra), (vs, ts) in data:
    assert len(vs) < 65536, name
    pos = b"".join(struct.pack("<3h", *(round((v[0][i] - centre[i]) / scale * 32767) for i in range(3))) for v in vs)
    nor = b"".join(struct.pack("<3b", *(max(-127, min(127, round(v[1][i] * 127))) for i in range(3))) for v in vs)
    col = b"".join(struct.pack("<4B", *v[2], v[3]) for v in vs)
    idx = b"".join(struct.pack("<3H", *t) for t in ts)
    blobs.append(pad4(pos) + pad4(nor) + pad4(col) + pad4(idx))
    pts = [v[0] for v in vs]
    bc = sum(pts, Vector()) / len(pts)
    j = {"name": name, "parent": parent, "pivot": [round(x, 4) for x in pivot] if pivot is not None else None,
         "verts": len(vs), "tris": len(ts), "bound": [round(bc.x, 4), round(bc.y, 4), round(bc.z, 4), round(max((p - bc).length for p in pts), 4)]}
    if name.startswith("plate:"):
        pl = dict(plates)[extra["label"]]
        mw = pl.matrix_world
        j["plate"] = {"label": extra["label"], "centre": [round(x, 4) for x in mw @ Vector((0, 0, 0.1))],
                      "axis": [round(x, 4) for x in (mw.to_3x3() @ Vector((0, 0, 1))).normalized()]}
    parts_json.append(j)

header = {"size": [RW, RH], "quant": {"centre": [round(x, 5) for x in centre], "scale": round(scale, 5)},
          "camera": {"view": [round(view[r][c], 6) for c in range(4) for r in range(4)], "ortho": 6.5},
          "lights": {"sun": [round(x, 4) for x in sun_dir], "sunColor": [1, .96, .9], "sunEnergy": 2.3,
                     "fill": [round(x, 4) for x in fill_dir], "fillColor": [.85, .92, 1], "sky": [1, .97, .92]},
          "shadows": [{"at": [0.06, -0.1], "r": [0.75, 0.6], "a": 0.32}, {"at": [1.7, -0.5], "r": [0.85, 0.6], "a": 0.26},
                      {"at": [-1.4, -0.5], "r": [0.8, 0.6], "a": 0.18}],
          "parts": parts_json}
hb = pad4(json.dumps(header, separators=(",", ":")).encode())
os.makedirs(os.path.dirname(OUT), exist_ok=True)
with open(OUT, "wb") as f:
    f.write(b"MZR1" + struct.pack("<I", len(hb)) + hb + b"".join(blobs))
print("robot:", os.path.relpath(OUT), os.path.getsize(OUT), "bytes,", sum(p["tris"] for p in parts_json), "triangles,", sum(p["verts"] for p in parts_json), "vertices")
for p in parts_json: print("  ", p["name"], p["verts"], p["tris"])
