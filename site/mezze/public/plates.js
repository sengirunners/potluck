// The robot's plates and the lower page's plates turn the same way. robot.js spins its plates at SPIN; scenes.js steps
// each plate's sheet (img/props/plate-*.webp, drawn by build/mezze-props.mjs) at the same speed, through TURNS turns
// laid out COLS to a row. Every one of those reads these numbers from here.
export const SPIN = 0.9; // radians a second
export const TURNS = 36; // turns in a plate's sheet, 10 degrees apart
export const COLS = 6; // turns to a row of the sheet
