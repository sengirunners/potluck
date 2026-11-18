"""The props of mezze.sh's lower page: the robot's six plates of tapas, a cloche, a phone, two laptops and the team's
home box with the robot's face and belly, the kitchen pass, a cazuela of olives and Potluck's blanket. They are built
from the robot's own shapes and colours (build/mezze_shapes.py). build/mezze-props.mjs runs this and draws the result
with robot.js's shader; run that, not this:

    blender -b --python build/mezze-props.py -- <out.json>

The file: {"props": {name: {"p": positions, "n": normals, "c": colours, "anchors": {name: [x, y, z]}}}}, three numbers
a corner (world space, z up, -y towards the camera) and four a corner for colour (sRGB 0-255 and the roughness byte,
as robot.bin stores it: build/mezze_shapes.py triangles()). Anchors are points the page places other things on.
"""
import bpy, bmesh, math, json, os, sys
from mathutils import Vector

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = sys.argv[sys.argv.index("--") + 1]
bpy.ops.wm.read_factory_settings(use_empty=True)
sc = bpy.context.scene
sys.dont_write_bytecode = True
sys.path.insert(0, HERE)
from mezze_shapes import *  # the helpers, the palette, the plate and the tapas (build/mezze_shapes.py)

def spun(name, prof, m, steps=28, loc=(0, 0, 0)):
    """A shape turned from its profile [(radius, height), ...] about z, as the plate is."""
    bm = bmesh.new()
    vs = [bm.verts.new((r, 0, z)) for r, z in prof]
    es = [bm.edges.new((vs[i], vs[i + 1])) for i in range(len(vs) - 1)]
    bmesh.ops.spin(bm, geom=vs + es, cent=(0, 0, 0), axis=(0, 0, 1), angle=math.tau, steps=steps)
    bmesh.ops.remove_doubles(bm, verts=bm.verts, dist=1e-4)
    bmesh.ops.recalc_face_normals(bm, faces=bm.faces)
    me = bpy.data.meshes.new(name); bm.to_mesh(me); bm.free()
    o = bpy.data.objects.new(name, me); sc.collection.objects.link(o); o.location = loc
    me.materials.append(m)
    for p in me.polygons: p.use_smooth = True
    return o

def rbox(loc, size, m, bevel, parent=None, segs=2, rot=(0, 0, 0)):
    """A box with rounder edges than box() gives."""
    o = box(loc, size, m, rot=rot, bevel=bevel, parent=parent)
    o.modifiers[0].segments = segs
    return o

def holder(name):
    E = bpy.data.objects.new(name, None); sc.collection.objects.link(E); return E

# The robot's pairings of rim and food: Ana's agents, Leo's agents, sessions, work items, events, mail.
RIMS = {"bravas": SAPH, "toast": EMER, "grapes": SAFF, "cheese": AMET, "prawns": TEAL, "olives": RUBY}
steel = mat("steel", "#C7D2D8", .1, spec=1.0)

PROPS = {}  # name -> (objects, anchors)
def prop(name, build):
    before = set(bpy.data.objects)
    anchors = build() or {}
    PROPS[name] = ([o for o in bpy.data.objects if o not in before], anchors)

def plate(food):
    def build():
        P = plate_mesh(RIMS[food]); FOODS[food](P)
        return {"centre": Vector((0, 0, 0.1))}
    return build

def cloche():
    prof = [(0.0, 0.62), (0.12, 0.61), (0.3, 0.55), (0.44, 0.43), (0.53, 0.28), (0.57, 0.12), (0.58, 0.03), (0.6, 0.0), (0.62, 0.02), (0.6, 0.05)]
    spun("dome", prof, steel, 32, (0, 0, 0.1))
    spun("band", [(0.6, 0.05), (0.615, 0.11), (0.6, 0.16), (0.585, 0.16)], terra, 32, (0, 0, 0.1))
    cyl((0, 0, 0.75), 0.035, 0.08, ink, 10)
    uvsphere((0, 0, 0.8), 0.075, terra, (1, 1, .8), None, 12)

def phone():
    P = holder("phone")
    rbox((0, 0, 0), (0.48, 0.05, 0.98), ink, 0.12, P, 3)
    rbox((0, -0.035, 0), (0.43, 0.02, 0.92), mat("glass", "#FBF7EE", .5), 0.08, P, 3)
    box((0, -0.06, 0.82), (0.11, 0.005, 0.025), ink, bevel=0.02, parent=P)
    box((-0.49, 0, 0.35), (0.012, 0.03, 0.09), terra, bevel=0.008, parent=P)
    P.rotation_euler = (math.radians(-12), 0, 0)
    bpy.context.view_layer.update()
    return {"screen-tl": P.matrix_world @ Vector((-0.43, -0.056, 0.92)), "screen-br": P.matrix_world @ Vector((0.43, -0.056, -0.92))}

def robot_face(P, x, y, z, w, h):
    """The robot's face on a screen: an ink panel, two glowing eyes and a mouth."""
    rbox((x, y, z), (w, 0.02, h), ink, min(w, h) * 0.25, P)
    for sx in (-1, 1):
        sphere((x + sx * w * 0.36, y - 0.03, z + h * 0.18), h * 0.2, eye, 2, (1, 0.45, 1), parent=P)
    limb_local(P, (x - w * 0.14, y - 0.03, z - h * 0.42), (x + w * 0.14, y - 0.03, z - h * 0.42), h * 0.06, eye)

def laptop(accent):
    def build():
        P = holder("laptop")
        rbox((0, 0, 0.04), (0.7, 0.45, 0.04), cream, 0.035, P)
        box((0, -0.05, 0.085), (0.6, 0.28, 0.006), mat("keys", "#E7DCC4", .6), bevel=0.004, parent=P)
        box((0, -0.36, 0.083), (0.16, 0.06, 0.004), mat("pad", "#EFE6D2", .4), parent=P)
        L = holder("lid"); L.parent = P; L.location = (0, 0.43, 0.06)
        rbox((0, 0, 0.45), (0.7, 0.03, 0.45), cream, 0.035, L)
        robot_face(L, 0, -0.035, 0.47, 0.6, 0.37)
        box((0, 0.035, 0.45), (0.7, 0.004, 0.45), mat("acc" + accent, accent, .15), parent=L)
        L.rotation_euler = (math.radians(-14), 0, 0)
    return build

def home():
    """The always-on machine: the robot's body as a box, its teal panel, buttons and red-ball antenna."""
    P = holder("home")
    rbox((0, 0, 0.55), (0.62, 0.5, 0.55), cream, 0.14, P, 3)
    rbox((0, -0.5, 0.58), (0.44, 0.03, 0.34), teal, 0.1, P, 3)
    sphere((0, -0.55, 0.7), 0.09, mat("btn", RUBY, .15, emit=1.2), 1, parent=P)
    for i, c in enumerate((SAFF, AMET)):
        sphere((-0.14 + 0.28 * i, -0.55, 0.42), 0.06, mat("d" + c, c, .15, emit=.5), 1, parent=P)
    box((0, 0, 0.04), (0.66, 0.54, 0.05), terra, bevel=0.04, parent=P)
    limb_local(P, (0.25, 0.1, 1.08), (0.32, 0.1, 1.45), 0.03, stick_m)
    sphere((0.33, 0.1, 1.52), 0.1, red, 2, parent=P)

def kitchen_pass():
    """The pass: a steel shelf on two posts under three heat lamps, over a plain counter: its front carries the page's
    labels, so nothing is drawn where they sit."""
    W = 5.6
    box((0, 0, 0.0), (W / 2, 0.36, 0.06), steel, bevel=0.03)
    for sx in (-1, 1):
        limb_local(None, (sx * (W / 2 - 0.2), 0.15, 0.0), (sx * (W / 2 - 0.2), 0.15, 2.0), 0.05, steel)
    box((0, 0.15, 2.02), (W / 2 - 0.05, 0.08, 0.04), steel, bevel=0.02)
    bulb = mat("bulb", "#FFC94A", .2, emit=3)
    for x in (-1.9, 0, 1.9):
        limb_local(None, (x, 0.15, 2.0), (x, 0.0, 1.75), 0.02, ink)
        spun("shade", [(0.0, 0.32), (0.08, 0.31), (0.2, 0.2), (0.3, 0.02), (0.32, 0.0), (0.3, -0.01), (0.0, 0.18)], terra, 24, (x, 0.0, 1.45))
        uvsphere((x, 0.0, 1.47), 0.11, bulb, (1, 1, .8), None, 12)
    rbox((0, -0.05, -0.75), (W / 2 - 0.25, 0.42, 0.7), mat("counter", "#F2D9AE", .3, spec=.8), 0.06)
    box((0, -0.05, -1.48), (W / 2 - 0.2, 0.44, 0.05), terra, bevel=0.03)
    anchors = {"plate%d" % i: Vector((-2.35 + i * 0.94, -0.05, 0.06)) for i in range(6)}
    anchors["labels"] = Vector((0, -0.5, -0.75))
    return anchors

def cazuela():
    """The robot's cazuela of olives, from its ground."""
    terracotta = mat("cazuela", "#D8582E", .35, spec=.6)
    R, H = 0.62, 0.2
    cz = spun("bowl", [(0.0, 0.02), (R * .8, 0.02), (R, H), (R * 1.04, H + .02), (R * .94, H + .02), (R * .78, H * .5), (0.0, H * .45)], terracotta, 24)
    cyl((0, 0, 0.12), 0.5, 0.03, mat("oil", "#E8541E", .1, spec=1.0), 24, False, parent=cz)
    for (x, y) in ((-0.2, 0.1), (0.15, 0.2), (0.2, -0.12), (-0.1, -0.2), (0.0, 0.0)):
        uvsphere((x, y, 0.18), 0.1, olive_g, (1.2, .9, .9), cz, 12)
        sphere((x + .08, y, 0.2), 0.04, red, 1, parent=cz)

def blanket():
    """Potluck's gingham blanket on a round of grass."""
    cyl((0, 0, -0.03), 2.6, 0.06, mat("grass", "#7DBB45", .6), 48, False)
    g_r, g_c, g_m = mat("gr", "#E8735F", .7), mat("gc", "#FFF6E3", .75), mat("gm", "#F2A79A", .7)
    N, S = 10, 0.24
    B = holder("blanket")
    for i in range(N):
        for j in range(N):
            a, c = i % 2, j % 2
            box(((i - N / 2 + .5) * S, (j - N / 2 + .5) * S, 0.012), (S / 2, S / 2, 0.012), g_r if (a and c) else g_c if not (a or c) else g_m, parent=B)
    B.rotation_euler = (0, 0, math.radians(22))
    for (x, y) in ((-2.0, 0.6), (1.9, -0.9), (-1.2, -1.6), (1.4, 1.5), (2.2, 0.4)):
        for d in (-0.05, 0, 0.05):
            limb_local(None, (x + d, y, 0.0), (x + d * 2.2, y, 0.16), 0.016, leaf)
    return {"p0": Vector((-0.55, 0.35, 0.03)), "p1": Vector((0.5, 0.4, 0.03)), "p2": Vector((-0.2, -0.5, 0.03)), "p3": Vector((0.7, -0.45, 0.03))}

for f in FOODS: prop("plate-" + f, plate(f))
prop("cloche", cloche); prop("phone", phone); prop("laptop-leo", laptop(EMER)); prop("laptop-ana", laptop(SAPH)); prop("home", home)
prop("pass", kitchen_pass); prop("cazuela", cazuela); prop("blanket", blanket)

# ---------- write ----------
bpy.context.view_layer.update()
dg = bpy.context.evaluated_depsgraph_get()
def flat(objs):
    """The props' triangles as flat lists: three numbers a corner for position and normal, four for colour (sRGB and
    the roughness byte, as robot.bin stores it)."""
    P, N, C = [], [], []
    for corners in triangles([o for o in objs if o.type == "MESH"], dg):
        for p, n, rgb, rough in corners:
            P += [round(p.x, 4), round(p.y, 4), round(p.z, 4)]; N += [round(n.x, 3), round(n.y, 3), round(n.z, 3)]
            C += [*rgb, rough]
    return {"p": P, "n": N, "c": C}

with open(OUT, "w") as f:
    json.dump({"props": {k: dict(flat(objs), anchors={a: list(v) for a, v in anchors.items()}) for k, (objs, anchors) in PROPS.items()}}, f, separators=(",", ":"))
print("props:", ", ".join(PROPS))
