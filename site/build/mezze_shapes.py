"""The shapes, colours and tapas of Mezze's picture, shared by the scripts that build its 3D files: build/mezze-robot.py
(the live robot) and build/mezze-props.py (the props of the lower page). Blender only; import it after the scene is
reset, since importing it makes the materials:

    sys.path.insert(0, os.path.dirname(os.path.abspath(__file__))); from mezze_shapes import *

A material keeps plain values (colour, roughness, glow) in its "mz" property: the files these scripts write carry those
values, and robot.js's shader lights them, so nothing here depends on Blender's own renderer.
"""
import bpy, bmesh, math, json
from mathutils import Vector


_mats = {}
def mat(name, color, rough=0.5, emit=0.0, spec=0.4, metal=0.0):
    if name in _mats: return _mats[name]
    m = bpy.data.materials.new(name)
    m["mz"] = json.dumps({"c": color, "r": rough, "e": emit})
    _mats[name] = m; return m

def put(o, m, flat=False, parent=None, loc=None):
    o.data.materials.clear(); o.data.materials.append(m)
    for p in o.data.polygons: p.use_smooth = not flat
    if parent: o.parent = parent
    if loc is not None: o.location = loc
    return o

def sphere(loc, r, m, sub=2, scale=(1,1,1), flat=True, parent=None, rot=(0,0,0)):
    bpy.ops.mesh.primitive_ico_sphere_add(subdivisions=sub, radius=r, location=loc)
    o = bpy.context.object; o.scale = scale; o.rotation_euler = rot
    return put(o, m, flat, parent)

def uvsphere(loc, r, m, scale=(1,1,1), parent=None, seg=24):
    seg = max(8, min(seg, 12))  # real time: fewer segments than the picture
    bpy.ops.mesh.primitive_uv_sphere_add(segments=seg, ring_count=seg // 2, radius=r, location=loc)
    o = bpy.context.object; o.scale = scale
    return put(o, m, False, parent)

def cyl(loc, r, h, m, verts=10, flat=True, rot=(0,0,0), parent=None):
    bpy.ops.mesh.primitive_cylinder_add(vertices=min(verts, 16), radius=r, depth=h, location=loc, rotation=rot)
    return put(bpy.context.object, m, flat, parent)

def box(loc, size, m, rot=(0,0,0), bevel=0.0, parent=None):
    bpy.ops.mesh.primitive_cube_add(location=loc, rotation=rot); o = bpy.context.object
    o.scale = size
    if bevel:
        bpy.context.view_layer.update()
        mod = o.modifiers.new("b", "BEVEL"); mod.width = bevel; mod.segments = 1
    return put(o, m, False, parent)

def limb(a, b, r, m, verts=8):
    a, b = Vector(a), Vector(b); d = b - a
    bpy.ops.mesh.primitive_cylinder_add(vertices=verts, radius=r, depth=d.length, location=(a + b) / 2)
    o = bpy.context.object
    o.rotation_euler = d.to_track_quat("Z", "Y").to_euler()
    return put(o, m, True)

CREAM = "#FFF6E3"
EMER, SAPH, RUBY, AMET, SAFF, TEAL, CORAL = "#10A66A", "#2D6CE6", "#D91C4A", "#8B3FD6", "#F7B514", "#0DB3AE", "#FF6A55"
INK = "#233231"
cream = mat("cream", CREAM, .16, spec=.9)
sage = mat("emer", EMER, .14, spec=1.0)
sky = mat("saph", SAPH, .12, spec=1.0)
terra = mat("coral", CORAL, .14, spec=1.0)
teal = mat("teal", TEAL, .12, spec=1.0)
ink = mat("ink", INK, .18, spec=.8)
eye = mat("eye", "#7FFFE6", .3, emit=6)
red = mat("red", RUBY, .1, spec=1.0)
stick_m = mat("stick", "#D9A441", .35, spec=.7)

# ---------- the plates and the tapas they carry ----------
def plate_mesh(rim_col):
    prof = [(0.0, 0.05), (0.3, 0.05), (0.4, 0.09), (0.52, 0.16), (0.56, 0.18), (0.6, 0.15), (0.58, 0.06), (0.48, 0.0), (0.25, 0.0), (0.0, 0.0)]
    bm = bmesh.new()
    vs = [bm.verts.new((r, 0, z)) for r, z in prof]
    es = [bm.edges.new((vs[i], vs[i + 1])) for i in range(len(vs) - 1)]
    bmesh.ops.spin(bm, geom=vs + es, cent=(0, 0, 0), axis=(0, 0, 1), angle=math.tau, steps=28)
    bmesh.ops.remove_doubles(bm, verts=bm.verts, dist=1e-4)
    bmesh.ops.recalc_face_normals(bm, faces=bm.faces)
    me = bpy.data.meshes.new("plate"); bm.to_mesh(me); bm.free()
    o = bpy.data.objects.new("plate", me); bpy.context.collection.objects.link(o)
    me.materials.append(mat("pl" + rim_col, rim_col, .1, spec=1.0)); me.materials.append(mat("well", "#FFFDF6", .12, spec=1.0))
    for p in me.polygons:
        c = p.center; r = math.hypot(c.x, c.y)
        p.material_index = 1 if (c.z > 0.03 and r < 0.4) else 0
        p.use_smooth = True
    return o

def limb_local(P, a, b, r, m):
    a, b = Vector(a), Vector(b); d = b - a
    bpy.ops.mesh.primitive_cylinder_add(vertices=6, radius=r, depth=d.length, location=(a + b) / 2)
    o = bpy.context.object; o.rotation_euler = d.to_track_quat("Z", "Y").to_euler()
    put(o, m, True, P)

olive_g = mat("olg", "#4FAF2E", .12, spec=1.0)
tomato = mat("tom", "#EE2A3A", .1, spec=1.0)
toast = mat("toast", "#E0A24A", .5)
grape = mat("grape", AMET, .1, spec=1.0)
cheese_m = mat("cheese", "#FFC21F", .3, spec=.7); hole = mat("hole", "#E0A010", .4)
leaf = mat("leaf", "#2FB04A", .3)
prawn_m, prawn_l = mat("prawn", "#FF7A4A", .2, spec=.9), mat("prawnl", "#FFB38A", .25)
spud, sauce, aioli = mat("spud", "#F4C04A", .45), mat("sauce", "#E8321F", .12, spec=1.0), mat("aioli", "#FFF3D2", .2, spec=.8)
lemon = mat("lemon", "#FFE24A", .2, spec=.9)

def food_bravas(P):
    cyl((0, 0, 0.065), 0.3, 0.03, sauce, 24, False, parent=P)
    for (x, y, r) in ((-0.15, 0.05, 20), (0.0, 0.14, 50), (0.12, 0.04, 10), (-0.04, -0.1, 70), (0.16, -0.12, 35), (-0.2, -0.1, 5)):
        box((x, y, 0.12), (0.062, 0.062, 0.055), spud, rot=(0.2, 0.1, math.radians(r)), bevel=0.02, parent=P)
    uvsphere((0.02, 0.0, 0.16), 0.1, aioli, (1, 1, .6), P, 16)
    uvsphere((0.02, 0.0, 0.2), 0.03, mat("pap", "#C8321C", .5), (1, 1, .6), P, 8)

def food_toast(P):
    for (x, y, rot) in ((-0.14, 0.1, 15), (0.14, -0.08, -20)):
        box((x, y, 0.1), (0.16, 0.13, 0.04), toast, rot=(0, 0, math.radians(rot)), bevel=0.03, parent=P)
        box((x, y, 0.15), (0.135, 0.105, 0.015), mat("ptom", "#F0301F", .2, spec=.9), rot=(0, 0, math.radians(rot)), bevel=0.01, parent=P)
        for dx, dy in ((-0.04, 0.0), (0.04, 0.03)):
            uvsphere((x + dx, y + dy, 0.17), 0.028, mat("seed", "#FFD8A0", .3), (1.3, 1, .5), P, 8)
        uvsphere((x + 0.05, y - 0.04, 0.18), 0.025, leaf, (1.6, .9, .5), P, 8)

def food_olives(P):
    for (x, y, rot) in ((-0.14, 0.06, 20), (0.1, 0.12, -25), (0.0, -0.14, 5), (0.2, -0.08, 40), (-0.12, -0.12, 70)):
        o = uvsphere((x, y, 0.15), 0.1, olive_g, (1.25, 0.9, 0.9), P, 20); o.rotation_euler.z = math.radians(rot)
        sphere((x + 0.1 * math.cos(math.radians(rot)), y + 0.1 * math.sin(math.radians(rot)), 0.16), 0.045, red, 1, parent=P)
    limb_local(P, (-0.1, -0.1, 0.2), (0.3, 0.05, 0.42), 0.012, stick_m)

def food_grapes(P):
    pts = [(-0.18, 0.0), (-0.06, 0.0), (0.06, 0.0), (0.18, 0.0), (-0.12, 0.1), (0.0, 0.1), (0.12, 0.1), (-0.06, 0.2), (0.06, 0.2),
           (-0.12, -0.1), (0.0, -0.1), (0.12, -0.1), (0.0, -0.2)]
    for (x, y) in pts:
        uvsphere((x, y - 0.02, 0.13 + (0.03 if abs(x) < 0.1 and abs(y) < 0.12 else 0)), 0.068, grape, parent=P, seg=10)
    sphere((0.2, 0.2, 0.15), 0.09, leaf, 1, (1.3, 0.8, 0.25), parent=P)

def food_cheese(P):
    for (x, y, rot) in ((-0.15, 0.08, 20), (0.18, -0.06, -160)):
        bpy.ops.mesh.primitive_cylinder_add(vertices=3, radius=0.21, depth=0.1, location=(x, y, 0.11))
        o = bpy.context.object; o.rotation_euler.z = math.radians(rot); put(o, cheese_m, True, P); o.scale = (1.0, 0.8, 1)
        uvsphere((x, y, 0.165), 0.024, hole, (1, 1, 0.5), P, 8)
    for (x, y) in ((0.0, -0.2), (0.12, 0.2), (-0.1, -0.12)):
        uvsphere((x, y, 0.15), 0.065, tomato, parent=P, seg=10)
        sphere((x, y, 0.2), 0.02, leaf, 1, (1.5, 1.5, 0.6), parent=P)

def food_prawns(P):
    for (cx, cy, rz) in ((-0.02, 0.1, 0), (0.03, -0.13, 180)):
        for k in range(7):
            a = k / 6 * math.pi * 1.05 + math.radians(rz)
            r = 0.075 - 0.007 * k
            uvsphere((cx + 0.17 * math.cos(a), cy + 0.12 * math.sin(a), 0.12 + 0.012 * k), r, prawn_m if k % 2 == 0 else prawn_l, (1, 1, .85), P, 10)
        a = math.pi * 1.05 + math.radians(rz)
        sphere((cx + 0.2 * math.cos(a), cy + 0.15 * math.sin(a), 0.2), 0.04, prawn_l, 1, (1.6, .5, .4), parent=P)
    cyl((0.2, 0.18, 0.09), 0.1, 0.035, lemon, 20, False, parent=P)
    uvsphere((-0.2, -0.18, 0.12), 0.04, leaf, (1.5, 1, .5), P, 8)

FOODS = {"bravas": food_bravas, "toast": food_toast, "olives": food_olives, "grapes": food_grapes, "cheese": food_cheese, "prawns": food_prawns}

# ---------- what the files carry ----------
def triangles(objs, dg):
    """Every triangle of the objects, evaluated (modifiers applied) in world space, as three corners of (position,
    normal, sRGB colour, roughness byte). The roughness byte is the material's roughness x200, at most 254, or 255
    for a glowing material: what robot.bin stores and robot.js's shader reads. An object with no material is an
    error, never a placeholder colour."""
    for o in objs:
        if not o.data.materials: raise ValueError("%s has no material" % o.name)
        ev = o.evaluated_get(dg)
        me = ev.to_mesh()
        me.calc_loop_triangles()
        mw = ev.matrix_world; nm = mw.to_3x3().inverted().transposed()
        mats = [json.loads(m["mz"]) for m in o.data.materials]
        corner = me.corner_normals
        for lt in me.loop_triangles:
            mm = mats[min(lt.material_index, len(mats) - 1)]
            c = mm["c"].lstrip("#"); rgb = tuple(int(c[i:i + 2], 16) for i in (0, 2, 4))
            rough = 255 if mm["e"] else min(254, round(mm["r"] * 200))
            yield [(mw @ me.vertices[lt.vertices[k]].co, (nm @ (corner[li].vector if lt.use_smooth else lt.normal)).normalized(), rgb, rough)
                   for k, li in enumerate(lt.loops)]
        ev.to_mesh_clear()

