#ifndef VOLTAGE_CONTROL_H
#define VOLTAGE_CONTROL_H

#include <Arduino.h>

// Sets up the PWM channel and pin for the adjustable converter
void setup_voltage_control();

// Sets the adjustable converter's output state (ON to preset voltage, or OFF)
void set_adjustable_converter_state(bool on);

#endif // VOLTAGE_CONTROL_H
