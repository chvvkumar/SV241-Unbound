#ifndef DEW_CONTROL_H
#define DEW_CONTROL_H

// Public function declarations

/**
 * @brief Initializes the dew heater controllers.
 * Sets up PWM channels and applies startup states.
 */
// Get the current mode of the dew heater (0=Manual, 1=Auto)
int get_dew_heater_mode(int heater_index);

void setup_dew_heaters();

/**
 * @brief Sets the enabled state of a specific dew heater.
 * 
 * @param heater_index The index of the heater (0 for PWM1, 1 for PWM2).
 * @param enabled True to enable the heater, false to disable it (sets PWM to 0).
 */
// Get the current power applied to the heater (0-100)
int get_heater_power(int heater_index);

void set_dew_heater_state(int heater_index, bool enabled);

/**
 * @brief Gets the current enabled state of a specific dew heater.
 *
 * @param heater_index The index of the heater.
 * @return true if the heater is enabled, false otherwise.
 */
bool get_dew_heater_state(int heater_index);

/**
 * @brief Gets the current power level of a specific dew heater.
 * 
 * @param heater_index The index of the heater.
 * @return The current power level in percent (0-100).
 */
// Set RAM override for manual PWM (0-100). -1 to release override (use config).
void set_dew_heater_pwm_ram(int heater_index, int pwm);

#endif // DEW_CONTROL_H
